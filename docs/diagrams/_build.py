#!/usr/bin/env python3
"""Generate the ai-memory-ops architecture diagrams as .excalidraw files.

Run: python3 docs/diagrams/_build.py
Everything is GENERIC (placeholders only) — this repo is public, so no
instance-specific hostnames/realms appear in the diagrams. Edit the data tables
in build_*() and re-run to regenerate; open the .excalidraw files at
https://excalidraw.com or in the VS Code Excalidraw extension.
"""
import json
import os

OUT = os.path.dirname(os.path.abspath(__file__))

# kind -> (stroke, fill)
COLORS = {
    "client":   ("#1971c2", "#e7f5ff"),
    "ingress":  ("#7048e8", "#f3f0ff"),
    "auth":     ("#e8590c", "#fff4e6"),
    "engine":   ("#2f9e44", "#ebfbee"),
    "storage":  ("#0c8599", "#e3fafc"),
    "idp":      ("#e03131", "#fff5f5"),
    "webhook":  ("#3b5bdb", "#edf2ff"),
    "job":      ("#f08c00", "#fff9db"),
    "external": ("#495057", "#f1f3f5"),
    "config":   ("#868e96", "#f8f9fa"),
    "registry": ("#343a40", "#f1f3f5"),
    "role":     ("#c2255c", "#fff0f6"),
    "step":     ("#1098ad", "#e3fafc"),
}
MUTED = "#495057"


def _txt_size(text, fs):
    lines = text.split("\n")
    w = max((len(l) for l in lines), default=1) * fs * 0.58
    h = len(lines) * fs * 1.25
    return w, h


class Diagram:
    def __init__(self, title):
        self.els = []
        self.rects = {}
        self.n = 0
        self.title = title
        self._add_title(title)

    def _nid(self, p):
        self.n += 1
        return f"{p}{self.n}"

    def _common(self, **kw):
        base = dict(
            angle=0, strokeColor="#1e1e1e", backgroundColor="transparent",
            fillStyle="solid", strokeWidth=1, strokeStyle="solid", roughness=0,
            opacity=100, groupIds=[], frameId=None, roundness=None,
            seed=self.n * 1000 + 7, versionNonce=self.n * 13 + 1, version=1,
            isDeleted=False, boundElements=[], updated=1, link=None, locked=False,
        )
        base.update(kw)
        return base

    def _add_title(self, title):
        w, h = _txt_size(title, 22)
        self.els.append(self._common(
            id=self._nid("title"), type="text", x=40, y=24, width=w, height=h,
            text=title, originalText=title, fontSize=22, fontFamily=2,
            textAlign="left", verticalAlign="top", containerId=None,
            lineHeight=1.25, baseline=18, autoResize=True, strokeColor="#1e1e1e",
        ))

    def note(self, x, y, text, fs=12, color=MUTED, mono=False):
        w, h = _txt_size(text, fs)
        self.els.append(self._common(
            id=self._nid("note"), type="text", x=x, y=y, width=w, height=h,
            text=text, originalText=text, fontSize=fs, fontFamily=3 if mono else 2,
            textAlign="left", verticalAlign="top", containerId=None,
            lineHeight=1.25, baseline=round(fs * 0.8), autoResize=True,
            strokeColor=color,
        ))

    def zone(self, x, y, w, h, label, color="#adb5bd"):
        rid = self._nid("zone")
        self.els.append(self._common(
            id=rid, type="rectangle", x=x, y=y, width=w, height=h,
            strokeColor=color, backgroundColor="transparent", strokeStyle="dashed",
            strokeWidth=1, roundness={"type": 3},
        ))
        lw, lh = _txt_size(label, 13)
        self.els.append(self._common(
            id=self._nid("zlbl"), type="text", x=x + 12, y=y + 8, width=lw, height=lh,
            text=label, originalText=label, fontSize=13, fontFamily=2,
            textAlign="left", verticalAlign="top", containerId=None,
            lineHeight=1.25, baseline=10, autoResize=True, strokeColor=color,
        ))

    def card(self, cid, x, y, w, h, title, body="", kind="config"):
        stroke, fill = COLORS.get(kind, COLORS["config"])
        gid = "g-" + cid
        rect = self._common(
            id=cid, type="rectangle", x=x, y=y, width=w, height=h,
            strokeColor=stroke, backgroundColor=fill, fillStyle="solid",
            strokeWidth=2, roundness={"type": 3}, groupIds=[gid],
        )
        self.els.append(rect)
        self.rects[cid] = rect
        tw, th = _txt_size(title, 15)
        self.els.append(self._common(
            id=self._nid("ct"), type="text", x=x + 12, y=y + 10, width=min(tw, w - 20),
            height=th, text=title, originalText=title, fontSize=15, fontFamily=2,
            textAlign="left", verticalAlign="top", containerId=None, lineHeight=1.25,
            baseline=12, autoResize=True, strokeColor="#1e1e1e", groupIds=[gid],
        ))
        if body:
            bw, bh = _txt_size(body, 11)
            self.els.append(self._common(
                id=self._nid("cb"), type="text", x=x + 12, y=y + 34,
                width=min(bw, w - 20), height=bh, text=body, originalText=body,
                fontSize=11, fontFamily=2, textAlign="left", verticalAlign="top",
                containerId=None, lineHeight=1.3, baseline=9, autoResize=True,
                strokeColor=MUTED, groupIds=[gid],
            ))

    def _center(self, cid):
        r = self.rects[cid]
        return r["x"] + r["width"] / 2, r["y"] + r["height"] / 2

    def _edge_point(self, cid, tx, ty):
        r = self.rects[cid]
        cx, cy = r["x"] + r["width"] / 2, r["y"] + r["height"] / 2
        dx, dy = tx - cx, ty - cy
        if dx == 0 and dy == 0:
            return cx, cy
        sx = (r["width"] / 2) / abs(dx) if dx else 1e9
        sy = (r["height"] / 2) / abs(dy) if dy else 1e9
        s = min(sx, sy)
        return cx + dx * s, cy + dy * s

    def arrow(self, a, b, label="", color=MUTED, dashed=False, waypoints=None,
              both=False, label_dy=-16):
        wps = waypoints or []
        first = wps[0] if wps else self._center(b)
        last = wps[-1] if wps else self._center(a)
        sx, sy = self._edge_point(a, *first)
        ex, ey = self._edge_point(b, *last)
        abs_pts = [(sx, sy)] + wps + [(ex, ey)]
        pts = [[round(px - sx, 2), round(py - sy, 2)] for px, py in abs_pts]
        xs = [p[0] for p in pts]
        ys = [p[1] for p in pts]
        aid = self._nid("arr")
        arrow = self._common(
            id=aid, type="arrow", x=round(sx, 2), y=round(sy, 2),
            width=round(max(xs) - min(xs), 2), height=round(max(ys) - min(ys), 2),
            strokeColor=color, strokeWidth=2,
            strokeStyle="dashed" if dashed else "solid",
            roundness={"type": 2}, points=pts, lastCommittedPoint=None,
            startBinding={"elementId": a, "focus": 0, "gap": 6},
            endBinding={"elementId": b, "focus": 0, "gap": 6},
            startArrowhead="arrow" if both else None, endArrowhead="arrow",
        )
        self.els.append(arrow)
        self.rects[a]["boundElements"].append({"id": aid, "type": "arrow"})
        self.rects[b]["boundElements"].append({"id": aid, "type": "arrow"})
        if label:
            mid = abs_pts[len(abs_pts) // 2]
            mx = (sx + ex) / 2 if len(abs_pts) == 2 else mid[0]
            my = (sy + ey) / 2 if len(abs_pts) == 2 else mid[1]
            lw, lh = _txt_size(label, 11)
            self.els.append(self._common(
                id=self._nid("al"), type="text", x=round(mx - lw / 2, 2),
                y=round(my + label_dy, 2), width=lw, height=lh, text=label,
                originalText=label, fontSize=11, fontFamily=2, textAlign="center",
                verticalAlign="top", containerId=None, lineHeight=1.25, baseline=9,
                autoResize=True, strokeColor=color,
            ))

    def check_overlaps(self):
        cards = list(self.rects.values())
        warned = []
        for i in range(len(cards)):
            for j in range(i + 1, len(cards)):
                a, b = cards[i], cards[j]
                if (a["x"] < b["x"] + b["width"] and a["x"] + a["width"] > b["x"]
                        and a["y"] < b["y"] + b["height"] and a["y"] + a["height"] > b["y"]):
                    warned.append((a["id"], b["id"]))
        return warned

    def save(self, name):
        ov = self.check_overlaps()
        if ov:
            print(f"  WARN {name}: overlapping cards: {ov}")
        scene = {
            "type": "excalidraw", "version": 2, "source": "ai-memory-ops/docs/diagrams/_build.py",
            "elements": self.els,
            "appState": {"viewBackgroundColor": "#ffffff", "gridSize": None},
            "files": {},
        }
        data = json.dumps(scene, indent=2)
        path = os.path.join(OUT, name)
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        print(f"  wrote {name}  ({len(self.els)} elements, {len(data)} bytes)")


# ---------------------------------------------------------------------------
# 00 — System overview (the one-glance onboarding picture)
# ---------------------------------------------------------------------------
def build_overview():
    d = Diagram("ai-memory-ops — System overview")
    d.note(40, 58, "Self-hosted long-term memory for AI agents: a Rust MCP server backed by a git wiki + SQLite, gated by OIDC (Keycloak), packaged as a Helm chart + container images.", 12)

    d.zone(30, 110, 300, 470, "Clients (outside the cluster)", "#1971c2")
    d.card("ov-agents", 55, 150, 250, 92, "AI coding agents", "Claude Code · Codex · OpenCode\nMCP tool calls + lifecycle hooks\n(Bearer JWT / device-flow)", "client")
    d.card("ov-browser", 55, 280, 250, 78, "Browser users", "/web SPA + /api/v1 (read-only)\ninteractive OIDC login", "client")
    d.card("ov-operator", 55, 400, 250, 92, "Operator / CI", "helm upgrade --install\nKeycloak bootstrap + per-dev grant\n(kubectl, GitLab runner)", "external")

    d.zone(360, 110, 740, 700, "Kubernetes cluster", "#7048e8")
    d.card("ov-ingress", 390, 165, 220, 96, "Ingress", "nginx OR Traefik (auto-detected)\nTLS, host + path routing\noptional base path (/wiki)", "ingress")
    d.card("ov-mcpauth", 660, 150, 210, 96, "mcp-auth (sidecar)", "validates Keycloak JWT (JWKS)\nchecks roles mcp:read/write\nserves RFC 9728 metadata", "auth")
    d.card("ov-oauth2", 660, 290, 210, 78, "oauth2-proxy", "interactive OIDC for /web\ncookie session -> bearer", "auth")
    d.card("ov-engine", 920, 175, 160, 150, "ai-memory engine", "Rust MCP server\n/mcp /web /api\n/admin /hook\nsingle writer actor", "engine")
    d.card("ov-storage", 920, 380, 160, 92, "PVC /data", "wiki/ (git repo)\ndb/ memory.sqlite (WAL)\nReadWriteOnce, 1 pod", "storage")
    d.card("ov-webhooks", 620, 470, 270, 96, "Admission chain (write-time)", "scope-guard (ACL) ->\ncontributors (attribution) ->\ngit-mirror (backup push)", "webhook")
    d.card("ov-cron", 390, 470, 200, 96, "CronJobs", "ETL: code repos -> wiki pages\ncapacity: PVC usage report", "job")

    d.zone(1130, 110, 280, 470, "External services", "#495057")
    d.card("ov-keycloak", 1155, 150, 230, 96, "Keycloak (OIDC IdP)", "realm: 3 clients + roles\nmcp:read/write via group\nissuer + JWKS", "idp")
    d.card("ov-gitbak", 1155, 290, 230, 78, "External git repo", "durable wiki mirror\n(SSH / HTTPS push)", "external")
    d.card("ov-sources", 1155, 410, 230, 92, "Source repos", "ETL input (GitLab/GitHub)\nshallow clone -> opencode\n-> markdown", "external")
    d.note(1155, 540, "Build & release of the\n6 images + Helm chart:\nsee diagram 05.", 11, "#343a40")

    d.arrow("ov-agents", "ov-ingress", "HTTPS /mcp /hook", "#1971c2")
    d.arrow("ov-browser", "ov-ingress", "HTTPS /web", "#1971c2")
    d.arrow("ov-operator", "ov-engine", "helm / kubectl", "#495057", dashed=True, waypoints=[(180, 775), (910, 775), (910, 250)])
    d.arrow("ov-ingress", "ov-mcpauth", "forwardAuth", "#e8590c")
    d.arrow("ov-ingress", "ov-oauth2", "auth /web", "#e8590c")
    d.arrow("ov-mcpauth", "ov-engine", "validated + bearer", "#2f9e44")
    d.arrow("ov-oauth2", "ov-engine", "session -> bearer", "#2f9e44")
    d.arrow("ov-mcpauth", "ov-keycloak", "JWKS", "#e03131", dashed=True)
    d.arrow("ov-oauth2", "ov-keycloak", "OIDC login", "#e03131", dashed=True, waypoints=[(1120, 329)])
    d.arrow("ov-engine", "ov-storage", "read / write", "#0c8599")
    d.arrow("ov-engine", "ov-webhooks", "write gate", "#3b5bdb", waypoints=[(900, 400), (820, 470)])
    d.arrow("ov-webhooks", "ov-gitbak", "push", "#495057", waypoints=[(1110, 518)])
    d.arrow("ov-cron", "ov-engine", "POST /admin/write-page", "#f08c00", waypoints=[(900, 455), (900, 250)], label_dy=-30)
    d.arrow("ov-cron", "ov-sources", "clone", "#f08c00", dashed=True, waypoints=[(490, 755), (1130, 755), (1130, 456)])
    d.save("00-overview.excalidraw")


# ---------------------------------------------------------------------------
# 01 — Runtime topology (what runs in the cluster)
# ---------------------------------------------------------------------------
def build_runtime():
    d = Diagram("ai-memory-ops — Runtime topology (Kubernetes)")
    d.note(40, 58, "One namespace. The engine is a single non-scalable pod (RWO PVC + SQLite WAL). Everything else is optional and toggled in values.yaml.", 12)

    d.zone(30, 110, 470, 430, "Main pod  (Deployment: replicas=1, strategy=Recreate)", "#2f9e44")
    d.card("rt-engine", 60, 165, 260, 150, "ai-memory engine (container)", "image: ai-memory (engine + SPA)\nport 49374 · uid 1000\n/mcp /web /api/v1 /admin/* /hook\nprobes: ai-memory status", "engine")
    d.card("rt-mcpauth", 350, 165, 130, 150, "mcp-auth\n(sidecar)", "JWT validator\nport 8081\nstateless\n(no /data)", "auth")
    d.card("rt-pvc", 60, 370, 200, 96, "PVC  /data", "wiki/  (git working tree)\ndb/memory.sqlite (WAL)\nReadWriteOnce  (default 20Gi)", "storage")
    d.card("rt-config", 290, 360, 195, 56, "ConfigMap", "WIKI_SOURCES_JSON, git author,\nlanguage, model, base path", "config")
    d.card("rt-secret", 290, 440, 195, 60, "Secret (external)", "AI_MEMORY_AUTH_TOKEN, HOOK_*\nGITLAB/OPENCODE/OPENAI keys", "config")

    d.card("rt-svc", 560, 215, 170, 92, "Service (ClusterIP)", "49374 -> engine\n8081 -> mcp-auth\nselector component=main", "ingress")

    d.zone(560, 360, 470, 250, "Admission webhooks  (optional Deployments + Services)", "#3b5bdb")
    d.card("rt-scope", 585, 405, 130, 88, "scope-guard", "per-user ACL\n(user,ws,project)\n200 allow / 403\nport 8080", "webhook")
    d.card("rt-contrib", 740, 405, 130, 88, "contributors", "append actor to\npage frontmatter\nattribution\nport 8080", "webhook")
    d.card("rt-mirror", 895, 405, 115, 88, "git-mirror", "push pages to\nexternal git\nreplicas=1\nport 8080", "webhook")

    d.zone(1070, 110, 330, 500, "Scheduled + external", "#f08c00")
    d.card("rt-etl", 1095, 165, 280, 110, "CronJob: ETL", "supercronic + opencode + skills\nclone sources -> markdown\nPOST /admin/write-page (+ /embed)\nephemeral pod, emptyDir scratch", "job")
    d.card("rt-cap", 1095, 300, 280, 70, "CronJob: capacity", "df/du on PVC (read-only)\nJSON usage report -> stdout", "job")
    d.card("rt-keycloak", 1095, 410, 280, 70, "Keycloak (external)", "OIDC issuer + JWKS\nrealm clients + roles + group", "idp")
    d.card("rt-extgit", 1095, 510, 280, 64, "External git repo", "wiki mirror target", "external")

    d.arrow("rt-svc", "rt-engine", "49374", "#7048e8", waypoints=[(600, 340), (190, 340)])
    d.arrow("rt-svc", "rt-mcpauth", "8081", "#7048e8")
    d.arrow("rt-mcpauth", "rt-engine", "upstream", "#2f9e44")
    d.arrow("rt-engine", "rt-pvc", "fs read/write", "#0c8599")
    d.arrow("rt-config", "rt-engine", "env", "#868e96", dashed=True)
    d.arrow("rt-secret", "rt-engine", "envFrom", "#868e96", dashed=True, waypoints=[(270, 470), (270, 335)])
    d.arrow("rt-engine", "rt-scope", "POST /admit  ->  chain (reject = abort)", "#3b5bdb", waypoints=[(285, 335), (285, 540), (650, 540)], label_dy=8)
    d.arrow("rt-scope", "rt-contrib", "then", "#3b5bdb")
    d.arrow("rt-contrib", "rt-mirror", "then", "#3b5bdb")
    d.arrow("rt-mirror", "rt-extgit", "git push", "#495057", waypoints=[(1010, 449), (1060, 449), (1060, 542)])
    d.arrow("rt-etl", "rt-svc", "POST /admin/write-page", "#f08c00", label_dy=-14)
    d.arrow("rt-cap", "rt-pvc", "read PVC (read-only)", "#f08c00", dashed=True, waypoints=[(1060, 335), (1060, 635), (160, 635)])
    d.arrow("rt-mcpauth", "rt-keycloak", "JWKS", "#e03131", dashed=True, waypoints=[(415, 138), (1050, 138), (1050, 445)])
    d.save("01-runtime-topology.excalidraw")


# ---------------------------------------------------------------------------
# 02 — Auth edge (how requests are authenticated)
# ---------------------------------------------------------------------------
def build_auth():
    d = Diagram("ai-memory-ops — Auth edge & request flows")
    d.note(40, 58, "Three caller types, three auth paths, one engine. mcp-auth gates machine/hook traffic; oauth2-proxy gates the browser. Both inject a validated identity upstream.", 12)

    d.card("au-machine", 40, 130, 230, 92, "1 · Machine (MCP)", "Claude Code / Codex tools\nAuthorization: Bearer <JWT>\npaths: /mcp", "client")
    d.card("au-browser", 40, 250, 230, 92, "2 · Browser (/web)", "human in a browser\ncookie session\npaths: /web, /api/v1", "client")
    d.card("au-hook", 40, 370, 230, 92, "3 · Hooks (headless)", "agent lifecycle hooks\nBearer JWT (device-flow)\nor static HOOK_AUTH_TOKEN\npaths: /hook, /handoff", "client")

    d.card("au-ingress", 330, 250, 190, 110, "Ingress", "nginx: auth_request\nTraefik: forwardAuth\n+ optional StripPrefix\nroutes by path", "ingress")

    d.card("au-mcpauth", 590, 150, 250, 150, "mcp-auth  /verify", "1. parse JWT (RS256)\n2. issuer == OIDC_ISSUER\n3. JWKS sig + exp (+ aud)\n4. role: /mcp->read, write->mcp:write\n200 ok / 401 (WWW-Authenticate)\n/ 403 missing_role", "auth")
    d.card("au-oauth2", 590, 360, 250, 110, "oauth2-proxy", "no cookie -> 302 to Keycloak\nAuth Code + PKCE\nencrypted session cookie\ninjects bearer upstream", "auth")
    d.card("au-meta", 590, 520, 250, 70, "RFC 9728 metadata", "/.well-known/oauth-protected-resource\nPUBLIC, no auth -> mcp-auth:8081", "step")

    d.card("au-engine", 920, 280, 200, 120, "ai-memory engine", "trusts injected headers:\nX-Memory-Actor-User/Sub/\nClient/Agent/Session\n(static AI_MEMORY_AUTH_TOKEN)", "engine")
    d.card("au-keycloak", 920, 110, 200, 96, "Keycloak (OIDC)", "issuer + JWKS\nclients: mcp / cli / oauth2-proxy\nroles via group ai-memory", "idp")

    d.arrow("au-machine", "au-ingress", "/mcp", "#1971c2", waypoints=[(300, 215), (320, 280)])
    d.arrow("au-browser", "au-ingress", "/web", "#1971c2")
    d.arrow("au-hook", "au-ingress", "/hook", "#1971c2", waypoints=[(300, 415), (320, 330)])
    d.arrow("au-ingress", "au-mcpauth", "machine + hook auth", "#e8590c", waypoints=[(560, 270), (580, 225)])
    d.arrow("au-ingress", "au-oauth2", "browser auth", "#e8590c", waypoints=[(560, 340), (580, 405)])
    d.arrow("au-mcpauth", "au-keycloak", "JWKS discovery", "#e03131", dashed=True)
    d.arrow("au-oauth2", "au-keycloak", "Auth Code + PKCE", "#e03131", dashed=True, waypoints=[(870, 365), (980, 206)])
    d.arrow("au-mcpauth", "au-engine", "200 + actor headers", "#2f9e44", waypoints=[(880, 270), (910, 300)])
    d.arrow("au-oauth2", "au-engine", "session + bearer", "#2f9e44", waypoints=[(880, 400), (910, 360)])
    d.arrow("au-meta", "au-mcpauth", "served by", "#1098ad", dashed=True)
    d.note(330, 430, "401 -> client reads metadata\n-> discovers Keycloak\n-> PKCE login -> retries", 11, "#0c8599")
    d.save("02-auth-flows.excalidraw")


# ---------------------------------------------------------------------------
# 03 — Keycloak realm model + developer onboarding
# ---------------------------------------------------------------------------
def build_keycloak():
    d = Diagram("ai-memory-ops — Keycloak realm & developer onboarding")
    d.note(40, 58, "One realm, three clients (one per auth style), two roles carried by a group. Bootstrap the realm once; then it is one grant per developer + a self-serve login.", 12)

    d.zone(30, 110, 560, 470, "Keycloak realm (set up once by kc-bootstrap.sh)", "#e03131")
    d.card("kc-oauth2", 55, 160, 240, 92, "client: ai-memory-oauth2-proxy", "confidential · standard flow\nPKCE S256\n-> gates /web (oauth2-proxy)", "idp")
    d.card("kc-mcp", 55, 275, 240, 92, "client: ai-memory-mcp", "public · standard flow + DCR\nPKCE S256\n-> MCP browser login", "idp")
    d.card("kc-cli", 55, 390, 240, 110, "client: ai-memory-cli", "public · DEVICE flow only · NO PKCE\n(separate client: PKCE would\nbreak device flow)\n-> CLI + headless hooks", "idp")
    d.card("kc-roles", 360, 175, 205, 80, "realm roles", "mcp:read\nmcp:write\n(mcp-auth requires both)", "role")
    d.card("kc-group", 360, 300, 205, 80, "group: ai-memory", "carries mcp:read + mcp:write\nrole->token mapper\n(realm_access.roles)", "role")
    d.card("kc-user", 360, 420, 205, 70, "developer (user)", "member of group ai-memory\n-> inherits both roles", "role")

    d.zone(630, 110, 780, 470, "Onboarding (two steps, per developer)", "#2f9e44")
    d.card("kc-bootstrap", 660, 160, 330, 110, "STEP 0 · bootstrap realm (once)", "kc-bootstrap.sh (idempotent)\nA) kubectl exec in Keycloak pod  (recommended)\nB) Helm hook Job (keycloakBootstrap.enabled)\ncreates 3 clients + roles + group", "step")
    d.card("kc-grant", 660, 305, 330, 110, "STEP 1 · operator grants access", "kc-add-user.sh  (per dev, inside the pod)\nadds user to group ai-memory\nverifies mcp:read+mcp:write IN EFFECT\nfail-loud, idempotent", "step")
    d.card("kc-self", 660, 450, 330, 110, "STEP 2 · developer self-onboards", "ai-memory auth login oidc-device\n   --client-id ai-memory-cli  (browser 1x)\nai-memory install-hooks --apply\n(no operator, no static token)", "step")

    d.card("kc-result", 1050, 305, 330, 110, "Result", "dev token (from MCP browser OR CLI\ndevice flow) carries mcp:read+write\n-> mcp-auth authorizes both /mcp and\n/hook for that user", "engine")

    d.arrow("kc-roles", "kc-group", "mapped to", "#c2255c")
    d.arrow("kc-group", "kc-user", "member -> inherits", "#c2255c")
    d.arrow("kc-cli", "kc-roles", "token carries", "#e03131", dashed=True, waypoints=[(320, 430), (345, 215)])
    d.arrow("kc-mcp", "kc-roles", "token carries", "#e03131", dashed=True, waypoints=[(330, 300), (345, 225)])
    d.arrow("kc-bootstrap", "kc-grant", "then", "#1098ad")
    d.arrow("kc-grant", "kc-self", "then", "#1098ad")
    d.arrow("kc-grant", "kc-user", "kcadm: add to group", "#2f9e44", dashed=True, waypoints=[(620, 360), (590, 450)])
    d.arrow("kc-self", "kc-cli", "device login", "#2f9e44", dashed=True, waypoints=[(620, 505), (300, 470)])
    d.arrow("kc-self", "kc-result", "unlocks", "#2f9e44", waypoints=[(1010, 480), (1110, 415)])
    d.save("03-keycloak-onboarding.excalidraw")


# ---------------------------------------------------------------------------
# 04 — Write path, admission chain & data pipelines
# ---------------------------------------------------------------------------
def build_data():
    d = Diagram("ai-memory-ops — Write path, admission chain & data pipelines")
    d.note(40, 58, "Reads hit SQLite directly. Writes pass a synchronous admission chain before they land; auxiliary jobs feed and back up the wiki. All optional pieces toggle in values.yaml.", 12)

    d.card("dt-writer", 40, 150, 200, 96, "Write source", "MCP write tool, /admin/write-page,\nconsolidate, delete, move-project,\nETL ingest", "client")
    d.card("dt-engine", 300, 150, 210, 130, "ai-memory engine", "Wiki::write_page\nbuilds {page, ctx:{user,\nworkspace,project,op}}\nsingle writer actor", "engine")

    d.zone(560, 110, 560, 300, "Admission chain  (in order, before persistence)", "#3b5bdb")
    d.card("dt-scope", 590, 160, 150, 110, "1 · scope-guard", "per-user ACL\nregex (ws, project)\n200 allow / 403 deny\nreject = abort write", "webhook")
    d.card("dt-contrib", 770, 160, 150, 110, "2 · contributors", "append actor to\nfrontmatter\n(agent|client key)\n204 if anonymous", "webhook")
    d.card("dt-mirror", 950, 160, 150, 110, "3 · git-mirror", "write to local tree\ncommit (actor author)\ndebounced push\n(async)", "webhook")
    d.note(590, 300, "failure_policy = reject, blocking, 2s timeout: any deny/timeout aborts the write atomically before it touches disk.", 11, "#3b5bdb")

    d.card("dt-sqlite", 300, 360, 210, 96, "SQLite + wiki", "persist (WAL)\nFTS5 index\nwiki/ git working tree\non /data PVC", "storage")
    d.card("dt-extgit", 950, 360, 150, 70, "External git", "durable mirror\n(audit trail)", "external")

    d.zone(30, 500, 1090, 300, "Auxiliary jobs (CronJobs)", "#f08c00")
    d.card("dt-etl", 60, 555, 320, 150, "ETL: code -> wiki", "schedule (default daily)\n1. clone sources (--depth 1)\n2. opencode -> markdown (wiki-ingest skill)\n3. wiki-lint (blocking gate)\n4. POST /admin/write-page (+ /embed)", "job")
    d.card("dt-sources", 430, 560, 200, 80, "Source repos", "GitLab / GitHub\n(WIKI_SOURCES_JSON)\nshallow clone", "external")
    d.card("dt-opencode", 430, 670, 200, 64, "opencode + LLM", "generates markdown\n(OPENCODE_API_KEY)", "external")
    d.card("dt-cap", 690, 560, 200, 96, "capacity monitor", "df/du on PVC (read-only)\nseverity ok/info/warn/crit\nper-project breakdown", "job")
    d.card("dt-obs", 940, 560, 160, 80, "Observability", "JSON on stdout\n-> logs / Prometheus\n(future)", "config")

    d.arrow("dt-writer", "dt-engine", "request", "#1971c2")
    d.arrow("dt-engine", "dt-scope", "POST /admit", "#3b5bdb", waypoints=[(540, 200), (575, 210)])
    d.arrow("dt-scope", "dt-contrib", "allow ->", "#3b5bdb")
    d.arrow("dt-contrib", "dt-mirror", "->", "#3b5bdb")
    d.arrow("dt-engine", "dt-sqlite", "persist (if allowed)", "#0c8599")
    d.arrow("dt-mirror", "dt-extgit", "git push", "#495057")
    d.arrow("dt-etl", "dt-sources", "clone", "#f08c00", dashed=True)
    d.arrow("dt-etl", "dt-opencode", "generate", "#f08c00", dashed=True)
    d.arrow("dt-etl", "dt-engine", "POST /admin/write-page", "#f08c00", waypoints=[(220, 560), (220, 215)])
    d.arrow("dt-cap", "dt-sqlite", "read-only", "#f08c00", dashed=True, waypoints=[(640, 500), (430, 458)])
    d.arrow("dt-cap", "dt-obs", "JSON", "#868e96", dashed=True)
    d.save("04-write-admission-data.excalidraw")


# ---------------------------------------------------------------------------
# 05 — Build / release pipeline + repo map
# ---------------------------------------------------------------------------
def build_build():
    d = Diagram("ai-memory-ops — Build, release & repo map")
    d.note(40, 58, "This repo is NOT the engine. It packages the ai-memory engine (Rust) + the ai-memory-ui SPA (SolidJS), each in its own repo, as 6 container images + a Helm chart, with a least-privilege deploy path.", 12)

    d.zone(30, 110, 690, 360, "CI: build-images.yml (GitHub Actions, matrix)", "#343a40")
    d.card("bd-trigger", 55, 160, 220, 110, "Triggers", "push main (images/** filter)\ntag v* (semver auto-tag)\nworkflow_dispatch\n  (engine_ref, tag)", "config")
    d.card("bd-buildx", 305, 175, 150, 80, "Buildx", "registry cache\n(:buildcache)\nlinux/amd64", "config")
    d.card("bd-images", 500, 130, 195, 320, "6 images", "ai-memory (engine + SPA)\nmcp-auth (JWT sidecar)\netl (code -> wiki)\ncontributors (webhook)\nscope-guard (webhook)\ngit-mirror (webhook)", "registry")
    d.card("bd-ghcr", 760, 230, 180, 96, "GHCR registry", "ghcr.io/<owner>/\nai-memory-ops/<img>:<tag>\ntags: latest, semver, sha", "registry")

    d.card("bd-chart", 760, 410, 180, 120, "Helm chart\nai-memory-svc", "renders: Deployment, Service,\nPVC, ConfigMap, Ingress,\nwebhooks, CronJobs,\nKeycloak bootstrap Job", "ingress")
    d.card("bd-deployer", 500, 540, 195, 96, "RBAC deployer", "memory-deployer SA\nleast-privilege Role\n(bootstrap, not in chart)", "config")
    d.card("bd-cluster", 760, 570, 180, 90, "Kubernetes", "helm upgrade --install\n-f values-<env>.yaml\n(overrides gitignored)", "engine")

    d.zone(990, 110, 420, 560, "Repo map (for first-time readers)", "#495057")
    d.card("bd-r1", 1015, 155, 370, 56, "images/", "6 Dockerfiles (one per image above)\n+ Go sources for the sidecars", "config")
    d.card("bd-r2", 1015, 225, 370, 70, "charts/ai-memory-svc/", "templates/, values.yaml, questions.yaml\n(Rancher UI), KEYCLOAK-BOOTSTRAP.md,\nscripts/ (kc-bootstrap, kc-add-user)", "config")
    d.card("bd-r3", 1015, 310, 370, 56, "examples/", "values-e2e-reference.yaml (sanitized)\nmcp-memory.json (.mcp.json template)", "config")
    d.card("bd-r4", 1015, 380, 370, 50, "deploy/", "rbac-deployer.yaml (deployer Role)", "config")
    d.card("bd-r5", 1015, 444, 370, 50, "docs/diagrams/", "these diagrams (.excalidraw + README)", "config")
    d.card("bd-r6", 1015, 508, 370, 56, ".github/workflows/", "build-images.yml (the CI above)", "config")
    d.card("bd-r7", 1015, 578, 370, 56, "CLAUDE.md", "operator runbook + ai-memory routing", "config")

    d.arrow("bd-trigger", "bd-buildx", "", "#868e96")
    d.arrow("bd-buildx", "bd-images", "builds", "#343a40")
    d.arrow("bd-images", "bd-ghcr", "push", "#343a40")
    d.arrow("bd-ghcr", "bd-chart", "pulled by", "#7048e8", dashed=True)
    d.arrow("bd-chart", "bd-cluster", "deploys", "#2f9e44")
    d.arrow("bd-deployer", "bd-cluster", "grants CRUD", "#868e96", dashed=True)
    d.save("05-build-release-repo-map.excalidraw")


def build_readme():
    txt = """# Architecture diagrams

Excalidraw diagrams of the **ai-memory-ops** architecture, ordered for a
first-time reader. Open any `.excalidraw` file at <https://excalidraw.com> (File
-> Open) or with the **Excalidraw** VS Code extension.

| # | File | What it answers |
|---|------|-----------------|
| 00 | [`00-overview.excalidraw`](00-overview.excalidraw) | The whole system at a glance: who calls it, what runs, what it talks to. **Start here.** |
| 01 | [`01-runtime-topology.excalidraw`](01-runtime-topology.excalidraw) | What actually runs in the cluster — the single pod, Service, PVC, webhooks, CronJobs. |
| 02 | [`02-auth-flows.excalidraw`](02-auth-flows.excalidraw) | How a request is authenticated — machine (JWT), browser (oauth2-proxy), hooks, RFC 9728 discovery. |
| 03 | [`03-keycloak-onboarding.excalidraw`](03-keycloak-onboarding.excalidraw) | The Keycloak realm (3 clients, roles, group) and the 2-step developer onboarding. |
| 04 | [`04-write-admission-data.excalidraw`](04-write-admission-data.excalidraw) | The write path + admission chain (scope-guard -> contributors -> git-mirror) and the ETL / capacity jobs. |
| 05 | [`05-build-release-repo-map.excalidraw`](05-build-release-repo-map.excalidraw) | CI that builds the 6 images, the Helm chart, the deploy path, and a map of this repo. |

## Conventions

- **Solid arrow** = primary request / data flow. **Dashed arrow** = secondary
  (discovery, config, async, read-only).
- Colour encodes role: blue = clients, violet = ingress/Service, orange =
  auth edge, green = engine, teal = storage, red = Keycloak/IdP, indigo =
  admission webhooks, amber = jobs, grey = external/config.
- Everything is **generic** — placeholders like `keycloak.example.com`,
  `<realm>`, `<owner>`. No instance-specific values live in this public repo
  (per-instance config lives in each deployment's own values file).

## Regenerating

The diagrams are generated from a single script so they stay consistent:

```sh
python3 docs/diagrams/_build.py
```

Edit the data tables in `_build.py` (nodes / edges / zones per diagram) and
re-run. You can also hand-edit a `.excalidraw` file in the editor and keep it;
the script overwrites, so fold manual changes back into `_build.py` if you want
them to survive a regen.
"""
    with open(os.path.join(OUT, "README.md"), "w") as f:
        f.write(txt)
    print("  wrote README.md")


if __name__ == "__main__":
    print("generating ai-memory-ops architecture diagrams...")
    build_overview()
    build_runtime()
    build_auth()
    build_keycloak()
    build_data()
    build_build()
    build_readme()
    print("done.")
