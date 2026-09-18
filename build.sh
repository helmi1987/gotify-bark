#!/usr/bin/env bash
#
# build.sh – baut das Bark-Forwarder-Plugin passend zur laufenden Gotify-Version.
#
# Gedacht für den Builder-Container aus docker-compose.yaml (gotify/build:<go>-linux-amd64),
# funktioniert aber auf jedem Linux mit Go, git, curl und gcc.
#
# Ablauf:
#   1. Gotify-Version ermitteln: von der laufenden Gotify (GET /version) oder aus GOTIFY_VERSION
#   2. Zu genau diesem Tag GO_VERSION und go.mod von github.com/gotify/server laden
#   3. Prüfen, ob die Go-Version hier im Container mit der von Gotify übereinstimmt
#      (sonst Abbruch mit dem passenden gotify/build-Image als Hinweis)
#   4. go.mod des Plugins mit gomod-cap auf die Gotify-Modulversionen abgleichen
#   5. Plugin bauen und nach OUT_DIR kopieren
#
# Umgebungsvariablen (alle optional):
#   GOTIFY_URL       Basis-URL der laufenden Gotify für die Versionsabfrage
#                    Standard: http://gotify:80   (Containername aus docker-compose.yaml)
#   GOTIFY_VERSION   Gotify-Tag fest vorgeben, z. B. v3.1.1 – überspringt die Abfrage
#   OUT_DIR          Zielordner für die .so, Standard: /out (Plugin-Ordner von Gotify)
#   OUT_NAME         Dateiname der .so, Standard: gotify-bark.so
#   SKIP_GO_CHECK=1  Go-Versionsprüfung überspringen (nur wenn du weisst, was du tust)
#   DRY_RUN=1        Nur prüfen und anzeigen, nichts bauen
#
# Beispiele:
#   ./build.sh                                  # Version von http://gotify:80 holen, bauen
#   GOTIFY_VERSION=v3.1.1 ./build.sh            # Version fest vorgeben
#   GOTIFY_URL=http://192.168.1.10:8888 ./build.sh
#   DRY_RUN=1 ./build.sh                        # nur prüfen
#   ./build.sh -h                               # diese Hilfe
#
set -euo pipefail

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

GOTIFY_URL="${GOTIFY_URL:-http://gotify:80}"
OUT_DIR="${OUT_DIR:-/out}"
OUT_NAME="${OUT_NAME:-gotify-bark.so}"
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
RAW="https://raw.githubusercontent.com/gotify/server"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFEHLER:\033[0m %s\n' "$*" >&2; exit 1; }

for tool in go curl; do
  command -v "$tool" >/dev/null || fail "$tool fehlt."
done

# --- 1. Gotify-Version -------------------------------------------------------------------
if [[ -n "${GOTIFY_VERSION:-}" ]]; then
  log "Gotify-Version aus GOTIFY_VERSION: $GOTIFY_VERSION"
else
  log "Frage Gotify-Version ab: $GOTIFY_URL/version"
  json="$(curl -fsS --max-time 10 "$GOTIFY_URL/version" 2>/dev/null)" \
    || fail "Gotify unter $GOTIFY_URL nicht erreichbar. GOTIFY_URL setzen oder GOTIFY_VERSION=vX.Y.Z vorgeben."
  GOTIFY_VERSION="$(printf '%s' "$json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
  [[ -n "$GOTIFY_VERSION" ]] || fail "Konnte die Version nicht aus der Antwort lesen: $json"
  log "Laufende Gotify meldet Version $GOTIFY_VERSION"
fi
[[ "$GOTIFY_VERSION" == v* ]] || GOTIFY_VERSION="v$GOTIFY_VERSION"

# --- 2. GO_VERSION und go.mod des Tags ---------------------------------------------------
log "Lade GO_VERSION und go.mod für gotify/server $GOTIFY_VERSION"
want_go="$(curl -fsS --max-time 20 "$RAW/$GOTIFY_VERSION/GO_VERSION" 2>/dev/null | tr -d '[:space:]')" \
  || fail "Tag $GOTIFY_VERSION nicht auf GitHub gefunden (https://github.com/gotify/server/tags)."
tmp_mod="$(mktemp)"
trap 'rm -f "$tmp_mod"' EXIT
curl -fsS --max-time 20 "$RAW/$GOTIFY_VERSION/go.mod" -o "$tmp_mod" || fail "go.mod für $GOTIFY_VERSION nicht ladbar."

# --- 3. Go-Version im Container prüfen ---------------------------------------------------
have_go="$(go env GOVERSION | sed 's/^go//')"
if [[ "$have_go" != "$want_go" ]]; then
  msg="Gotify $GOTIFY_VERSION ist mit Go $want_go gebaut, hier läuft Go $have_go.
       Ein damit gebautes Plugin würde Gotify beim Start abbrechen lassen.
       Richtiges Builder-Image: gotify/build:${want_go}-linux-amd64
       (in docker-compose.yaml beim Dienst plugin-builder eintragen, Container neu erstellen)."
  if [[ "${SKIP_GO_CHECK:-0}" == "1" ]]; then
    printf '\033[1;33mWARNUNG:\033[0m %s\n' "$msg (SKIP_GO_CHECK=1, weiter)" >&2
  else
    fail "$msg"
  fi
else
  log "Go-Version passt: $have_go"
fi

# --- 4. go.mod abgleichen ----------------------------------------------------------------
cd "$SRC_DIR"
if ! command -v gomod-cap >/dev/null; then
  log "Installiere gomod-cap"
  go install github.com/gotify/plugin-api/cmd/gomod-cap@latest
  export PATH="$PATH:$(go env GOPATH)/bin"
fi
log "Gleiche go.mod auf die Modulversionen von Gotify $GOTIFY_VERSION ab"
gomod-cap -from "$tmp_mod" -to go.mod >/dev/null 2>&1 || fail "gomod-cap fehlgeschlagen."
go mod tidy
if git -C "$SRC_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1 && ! git -C "$SRC_DIR" diff --quiet -- go.mod go.sum; then
  printf '\033[1;33mHINWEIS:\033[0m go.mod/go.sum wurden geändert – Gotify %s hat andere Modulversionen als das Repo. Diese Änderung committen.\n' "$GOTIFY_VERSION"
fi

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  log "DRY_RUN=1 – alles passt, es wird nicht gebaut."
  exit 0
fi

# --- 5. Bauen -----------------------------------------------------------------------------
log "Baue Plugin (CGO, -buildmode=plugin, -w -s)"
CGO_ENABLED=1 go build -a -installsuffix cgo -ldflags "-w -s" -buildmode=plugin -o "$OUT_NAME" .

plugin_version="$(grep -a -o 'Bark Forwarder v[0-9][0-9.]*' "$OUT_NAME" | head -1 || true)"
mkdir -p "$OUT_DIR"
cp "$OUT_NAME" "$OUT_DIR/$OUT_NAME"
log "Fertig: $OUT_DIR/$OUT_NAME  (${plugin_version:-Version nicht gefunden}, für Gotify $GOTIFY_VERSION / Go $want_go)"
log "Jetzt Gotify neu starten: docker compose restart gotify"
