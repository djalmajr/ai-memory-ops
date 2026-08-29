// Consumer keys (amk_) — optional SQLite-backed issuance for CLI/agent
// credentials. Disabled when KEYS_DB is empty so existing deployments stay
// byte-identical until opted in.
//
// Digest is SHA-256, not argon2/bcrypt: the secret is 160 bits of CSPRNG
// (not a password), /verify must look it up in O(1) on every request, and
// the engine hashes user tokens the same way.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "modernc.org/sqlite"
)

const (
	consumerKeyPrefix  = "amk_"
	consumerKeyEntropy = 20 // 160 bits → 40 hex chars
	consumerKeyPreview = "amk_\u2026"
)

var (
	consumerKeys             *consumerKeyStore
	actorProxyBearerToken    string
	passthroughUnknownBearer bool
	keysAdminSubjects        []adminSubjectPair

	keyIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

	errInvalidConsumerKey = errors.New("invalid consumer key")
)

var allowedScopes = []string{"read", "write", "admin"}

type adminSubjectPair struct {
	issuer, subject string
}

type consumerKeyStore struct {
	db *sql.DB
}

type keyOwner struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Issuer  string `json:"issuer,omitempty"`
	Subject string `json:"subject,omitempty"`
}

type caller struct {
	owner    keyOwner
	canIssue bool
}

type consumerKeyRecord struct {
	ID         string
	Preview    string
	ActorUser  string
	Scopes     []string
	Owner      keyOwner
	CreatedAt  int64
	ExpiresAt  *int64
	RevokedAt  *int64
	LastUsedAt *int64
	KeySHA256  string
	KeyLast4   string
}

type consumerKeyJSON struct {
	ID         string   `json:"id"`
	Key        string   `json:"key,omitempty"`
	Preview    string   `json:"preview"`
	ActorUser  string   `json:"actor_user"`
	Scopes     []string `json:"scopes"`
	Owner      keyOwner `json:"owner"`
	CreatedAt  int64    `json:"created_at"`
	ExpiresAt  *int64   `json:"expires_at"`
	RevokedAt  *int64   `json:"revoked_at"`
	LastUsedAt *int64   `json:"last_used_at"`
}

func (rec consumerKeyRecord) asJSON(plaintext string) consumerKeyJSON {
	scopes := rec.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return consumerKeyJSON{
		ID:         rec.ID,
		Key:        plaintext,
		Preview:    rec.Preview,
		ActorUser:  rec.ActorUser,
		Scopes:     scopes,
		Owner:      rec.Owner,
		CreatedAt:  rec.CreatedAt,
		ExpiresAt:  rec.ExpiresAt,
		RevokedAt:  rec.RevokedAt,
		LastUsedAt: rec.LastUsedAt,
	}
}

func initKeys() {
	passthroughUnknownBearer = os.Getenv("PASSTHROUGH_UNKNOWN_BEARER") == "1"
	keysAdminSubjects = parseAdminSubjects(os.Getenv("KEYS_ADMIN_SUBJECTS"))

	path := os.Getenv("KEYS_DB")
	if path == "" {
		return
	}
	token := os.Getenv("ACTOR_PROXY_BEARER_TOKEN")
	if token == "" {
		logger.Error("actor_proxy_bearer_missing",
			"hint", "ACTOR_PROXY_BEARER_TOKEN is required when KEYS_DB is set")
		os.Exit(1)
	}
	store, err := openKeysStore(path)
	if err != nil {
		logger.Error("keys_db_open_failed", "error", err, "path", path)
		os.Exit(1)
	}
	consumerKeys = store
	actorProxyBearerToken = token
	logger.Info("keys_enabled",
		"path", path,
		"passthrough_unknown_bearer", passthroughUnknownBearer,
		"admin_subjects", len(keysAdminSubjects),
	)
}

func parseAdminSubjects(s string) []adminSubjectPair {
	if s == "" {
		return nil
	}
	var out []adminSubjectPair
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		iss, sub, ok := strings.Cut(part, "|")
		if !ok || iss == "" || sub == "" {
			logger.Warn("keys_admin_subjects_skip", "entry", part)
			continue
		}
		out = append(out, adminSubjectPair{issuer: iss, subject: sub})
	}
	return out
}

func openKeysStore(path string) (*consumerKeyStore, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS consumer_keys (
  id TEXT PRIMARY KEY,
  key_sha256 TEXT UNIQUE NOT NULL,
  key_last4 TEXT NOT NULL,
  actor_user TEXT NOT NULL,
  scopes TEXT NOT NULL,
  owner_kind TEXT NOT NULL,
  owner_user TEXT,
  owner_issuer TEXT,
  owner_subject TEXT,
  owner_label TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  revoked_at INTEGER,
  last_used_at INTEGER
);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &consumerKeyStore{db: db}, nil
}

func (s *consumerKeyStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func generateSecret() (string, error) {
	b := make([]byte, consumerKeyEntropy)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return consumerKeyPrefix + hex.EncodeToString(b), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func previewOf(secret string) (last4, preview string) {
	if len(secret) >= 4 {
		last4 = secret[len(secret)-4:]
	}
	return last4, consumerKeyPreview + last4
}

func (s *consumerKeyStore) insert(rec consumerKeyRecord) error {
	_, err := s.db.Exec(`
INSERT INTO consumer_keys (
  id, key_sha256, key_last4, actor_user, scopes,
  owner_kind, owner_user, owner_issuer, owner_subject, owner_label,
  created_at, expires_at, revoked_at, last_used_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.KeySHA256, rec.KeyLast4, rec.ActorUser, strings.Join(rec.Scopes, ","),
		rec.Owner.Kind, nilString(ownerUser(rec.Owner)), nilString(rec.Owner.Issuer), nilString(rec.Owner.Subject), rec.Owner.Label,
		rec.CreatedAt, nullInt(rec.ExpiresAt), nullInt(rec.RevokedAt), nullInt(rec.LastUsedAt),
	)
	return err
}

const keyColumns = `id, key_sha256, key_last4, actor_user, scopes,
  owner_kind, owner_user, owner_issuer, owner_subject, owner_label,
  created_at, expires_at, revoked_at, last_used_at`

func (s *consumerKeyStore) getByID(id string) (*consumerKeyRecord, error) {
	return s.scanOne(`SELECT `+keyColumns+` FROM consumer_keys WHERE id = ?`, id)
}

func (s *consumerKeyStore) lookupBySecret(secret string) (*consumerKeyRecord, error) {
	return s.scanOne(`SELECT `+keyColumns+` FROM consumer_keys WHERE key_sha256 = ?`, hashSecret(secret))
}

func (s *consumerKeyStore) list() ([]consumerKeyRecord, error) {
	rows, err := s.db.Query(`SELECT ` + keyColumns + ` FROM consumer_keys ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]consumerKeyRecord, 0)
	for rows.Next() {
		rec, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *consumerKeyStore) revoke(id string) error {
	_, err := s.db.Exec(
		`UPDATE consumer_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), id,
	)
	return err
}

func (s *consumerKeyStore) touchLastUsed(id string) {
	if s == nil {
		return
	}
	_, err := s.db.Exec(`UPDATE consumer_keys SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id)
	if err != nil && logger != nil {
		logger.Error("keys_touch_last_used_failed", "error", err, "id", id)
	}
}

type scanner interface {
	Scan(dest ...any) error
}

func (s *consumerKeyStore) scanOne(q string, arg any) (*consumerKeyRecord, error) {
	rec, err := scanKey(s.db.QueryRow(q, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func scanKey(row scanner) (consumerKeyRecord, error) {
	var rec consumerKeyRecord
	var scopes string
	var ownerUser, ownerIssuer, ownerSubject sql.NullString
	var expiresAt, revokedAt, lastUsedAt sql.NullInt64
	err := row.Scan(
		&rec.ID, &rec.KeySHA256, &rec.KeyLast4, &rec.ActorUser, &scopes,
		&rec.Owner.Kind, &ownerUser, &ownerIssuer, &ownerSubject, &rec.Owner.Label,
		&rec.CreatedAt, &expiresAt, &revokedAt, &lastUsedAt,
	)
	if err != nil {
		return rec, err
	}
	rec.Scopes = splitScopes(scopes)
	rec.Preview = consumerKeyPreview + rec.KeyLast4
	rec.Owner.Issuer = ownerIssuer.String
	rec.Owner.Subject = ownerSubject.String
	rec.ExpiresAt = intPtr(expiresAt)
	rec.RevokedAt = intPtr(revokedAt)
	rec.LastUsedAt = intPtr(lastUsedAt)
	return rec, nil
}

func splitScopes(csv string) []string {
	if csv == "" {
		return []string{}
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normalizeScopes(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !slices.Contains(allowedScopes, s) {
			return nil, errors.New("invalid scope")
		}
		seen[s] = true
	}
	if len(seen) == 0 {
		return nil, errors.New("scopes required")
	}
	out := make([]string, 0, len(seen))
	for _, s := range allowedScopes {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

func ownerUser(o keyOwner) string {
	if o.Kind == "user" {
		return o.Label
	}
	return ""
}

func nilString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func intPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func handleKeys(w http.ResponseWriter, r *http.Request) {
	if consumerKeys == nil {
		http.NotFound(w, r)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/keys"), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		handleListKeys(w, r)
	case rest == "" && r.Method == http.MethodPost:
		handleCreateKey(w, r)
	case rest == "whoami" && r.Method == http.MethodGet:
		handleWhoami(w, r)
	case rest != "" && r.Method == http.MethodDelete:
		handleRevokeKey(w, r, rest)
	default:
		if rest == "" || rest == "whoami" {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		http.NotFound(w, r)
	}
}

func handleWhoami(w http.ResponseWriter, r *http.Request) {
	ident, err := callerIdentity(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"identity": nil, "can_issue": false})
		return
	}
	if ident == nil {
		writeJSON(w, http.StatusOK, map[string]any{"identity": nil, "can_issue": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity": ident.owner, "can_issue": ident.canIssue})
}

func requireIssuer(w http.ResponseWriter, r *http.Request) *caller {
	ident, err := callerIdentity(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, err.Error())
		return nil
	}
	if ident == nil {
		writeJSONError(w, http.StatusForbidden, "issuance requires an identified operator")
		return nil
	}
	if !ident.canIssue {
		writeJSONError(w, http.StatusForbidden, "issuance requires mcp:admin")
		return nil
	}
	return ident
}

func handleListKeys(w http.ResponseWriter, r *http.Request) {
	if requireIssuer(w, r) == nil {
		return
	}
	recs, err := consumerKeys.list()
	if err != nil {
		logger.Error("keys_list_failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	keys := make([]consumerKeyJSON, 0, len(recs))
	for _, rec := range recs {
		keys = append(keys, rec.asJSON(""))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func handleCreateKey(w http.ResponseWriter, r *http.Request) {
	ident := requireIssuer(w, r)
	if ident == nil {
		return
	}
	defer r.Body.Close()
	body, err := readJSONBody(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	for _, k := range []string{"owner", "owner_kind", "owner_user", "owner_issuer", "owner_subject", "owner_label"} {
		if _, ok := raw[k]; ok {
			writeJSONError(w, http.StatusBadRequest, "owner is derived from the caller, not the body")
			return
		}
	}
	var req struct {
		ID        string   `json:"id"`
		ActorUser string   `json:"actor_user"`
		Scopes    []string `json:"scopes"`
		ExpiresAt *int64   `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !keyIDRe.MatchString(req.ID) {
		writeJSONError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if strings.TrimSpace(req.ActorUser) == "" {
		writeJSONError(w, http.StatusBadRequest, "actor_user required")
		return
	}
	scopes, err := normalizeScopes(req.Scopes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ExpiresAt != nil && *req.ExpiresAt <= time.Now().Unix() {
		writeJSONError(w, http.StatusBadRequest, "expires_at is in the past")
		return
	}
	existing, err := consumerKeys.getByID(req.ID)
	if err != nil {
		logger.Error("keys_get_failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if existing != nil {
		writeJSONError(w, http.StatusConflict, "key id already exists")
		return
	}
	secret, err := generateSecret()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	last4, preview := previewOf(secret)
	rec := consumerKeyRecord{
		ID:        req.ID,
		Preview:   preview,
		ActorUser: req.ActorUser,
		Scopes:    scopes,
		Owner:     ident.owner,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: req.ExpiresAt,
		KeySHA256: hashSecret(secret),
		KeyLast4:  last4,
	}
	if err := consumerKeys.insert(rec); err != nil {
		if isUniqueConstraint(err) {
			writeJSONError(w, http.StatusConflict, "key id already exists")
			return
		}
		logger.Error("keys_insert_failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, rec.asJSON(secret))
}

func handleRevokeKey(w http.ResponseWriter, r *http.Request, id string) {
	if requireIssuer(w, r) == nil {
		return
	}
	if err := consumerKeys.revoke(id); err != nil {
		logger.Error("keys_revoke_failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readJSONBody(r *http.Request) ([]byte, error) {
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func callerIdentity(r *http.Request) (*caller, error) {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return nil, nil
	}
	tokenStr := strings.TrimPrefix(auth, prefix)

	if hookAuthToken != "" && subtle.ConstantTimeCompare([]byte(tokenStr), []byte(hookAuthToken)) == 1 {
		return nil, nil
	}

	if consumerKeys != nil && strings.HasPrefix(tokenStr, consumerKeyPrefix) {
		rec, err := consumerKeys.lookupBySecret(tokenStr)
		if err != nil {
			logger.Error("keys_lookup_failed", "error", err)
			return nil, errInvalidConsumerKey
		}
		if rec == nil || rec.RevokedAt != nil || expired(rec, time.Now().Unix()) {
			return nil, errInvalidConsumerKey
		}
		return &caller{owner: rec.Owner, canIssue: slices.Contains(rec.Scopes, "admin")}, nil
	}

	claims, _, err := parseJWT(tokenStr)
	if err != nil {
		return nil, nil
	}
	if oidcAud != "" && !claimContains(claims, "aud", oidcAud) {
		return nil, nil
	}
	owner := ownerFromJWT(claims)
	if owner == nil {
		return nil, nil
	}
	return &caller{owner: *owner, canIssue: jwtCanIssue(claims)}, nil
}

func ownerFromJWT(claims jwt.MapClaims) *keyOwner {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil
	}
	iss, _ := claims["iss"].(string)
	label, _ := claims["preferred_username"].(string)
	if label == "" {
		label = sub
	}
	return &keyOwner{Kind: "subject", Label: label, Issuer: iss, Subject: sub}
}

func jwtCanIssue(claims jwt.MapClaims) bool {
	if hasRealmRole(claims, "mcp:admin") {
		return true
	}
	iss, _ := claims["iss"].(string)
	sub, _ := claims["sub"].(string)
	for _, p := range keysAdminSubjects {
		if p.issuer == iss && p.subject == sub {
			return true
		}
	}
	return false
}

func expired(rec *consumerKeyRecord, now int64) bool {
	return rec.ExpiresAt != nil && *rec.ExpiresAt <= now
}

func stripQuery(uri string) string {
	if i := strings.Index(uri, "?"); i >= 0 {
		return uri[:i]
	}
	return uri
}

// Engine /admin/* (Capability::Admin), not /mcp/admin/ which is an MCP write path.
func isEngineAdminPath(uri string) bool {
	p := stripQuery(uri)
	if i := strings.Index(p, "/mcp/"); i >= 0 {
		p = p[:i]
	}
	return strings.Contains(p, "/admin/") || strings.HasSuffix(p, "/admin")
}

// keyAllowsRoute is honest about what forwardAuth can see (method + path).
// POST /mcp is JSON-RPC: memory_query and memory_write_page share the same
// path, and this hop does not get a reliable body, so any MCP call needs write.
func keyAllowsRoute(scopes []string, method, uri string) bool {
	hasAdmin := slices.Contains(scopes, "admin")
	hasWrite := hasAdmin || slices.Contains(scopes, "write")
	hasRead := hasWrite || slices.Contains(scopes, "read")

	if isEngineAdminPath(uri) {
		return hasAdmin
	}
	if isHookPath(uri) {
		return hasWrite
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return hasRead
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return hasWrite
	default:
		return hasAdmin
	}
}

// verifyConsumerKey handles amk_ bearers when the keys store is enabled.
// Returns true when the request was fully handled (success or rejection).
func verifyConsumerKey(w http.ResponseWriter, tokenStr, method, uri, ip string, start time.Time) bool {
	if consumerKeys == nil || !strings.HasPrefix(tokenStr, consumerKeyPrefix) {
		return false
	}
	rec, err := consumerKeys.lookupBySecret(tokenStr)
	if err != nil {
		logger.Error("verify_unauthorized",
			"reason", "consumer_key_lookup_failed",
			"error", err,
			"uri", uri,
			"ip", ip,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	now := time.Now().Unix()
	reason := ""
	switch {
	case rec == nil:
		reason = "consumer_key_unknown"
	case rec.RevokedAt != nil:
		reason = "consumer_key_revoked"
	case expired(rec, now):
		reason = "consumer_key_expired"
	}
	if reason != "" {
		logger.Info("verify_unauthorized",
			"reason", reason,
			"uri", uri,
			"ip", ip,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	if !keyAllowsRoute(rec.Scopes, method, uri) {
		logger.Info("verify_forbidden",
			"reason", "consumer_key_scope",
			"key_id", rec.ID,
			"method", method,
			"uri", uri,
			"ip", ip,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	go consumerKeys.touchLastUsed(rec.ID)
	logger.Info("verify_ok",
		"mode", "consumer_key",
		"key_id", rec.ID,
		"method", method,
		"uri", uri,
		"ip", ip,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
	// Set (never Add): Traefik copies these onto the upstream request. A
	// partial OIDC pair makes the engine 400, so issuer+sub go out together
	// or not at all — and only for admin keys owned by a subject.
	w.Header().Del("X-Memory-Actor-User")
	w.Header().Del("X-Memory-Actor-Issuer")
	w.Header().Del("X-Memory-Actor-Sub")
	w.Header().Set("Authorization", "Bearer "+actorProxyBearerToken)
	w.Header().Set("X-Memory-Actor-User", rec.ActorUser)
	if slices.Contains(rec.Scopes, "admin") && rec.Owner.Kind == "subject" && rec.Owner.Issuer != "" && rec.Owner.Subject != "" {
		w.Header().Set("X-Memory-Actor-Issuer", rec.Owner.Issuer)
		w.Header().Set("X-Memory-Actor-Sub", rec.Owner.Subject)
	}
	w.WriteHeader(http.StatusOK)
	return true
}
