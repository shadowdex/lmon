#!/usr/bin/env bash
# Tests install.sh against a local fake release server. Builds lmon for the
# host, so it needs Go, curl and python3. Run:  scripts/test-install.sh
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
SRV=""
cleanup() { if [ -n "$SRV" ]; then kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; fi; rm -rf "$WORK"; }
trap cleanup EXIT

# On GitHub Actions, "::error::" lines become check-run annotations, which can be
# read through the API without a login: failures stay diagnosable without the raw log.
annotate() { # LEVEL MESSAGE...
  [ -n "${GITHUB_ACTIONS:-}" ] || return 0
  local level=$1; shift
  local msg=$*
  msg=${msg//'%'/'%25'}; msg=${msg//$'\r'/'%0D'}; msg=${msg//$'\n'/'%0A'}
  echo "::$level title=install tests::${msg:0:1500}"
}
fatal() { echo "FATAL: $*"; annotate error "FATAL: $*"; exit 2; }

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok    $1"; }
bad() {
  fail=$((fail + 1)); echo "  FAIL  $1"
  [ -n "${2:-}" ] && echo "$2" | sed 's/^/          /'
  annotate error "FAIL: $1${2:+ :: $2}"
}
annotate notice "env: bash $BASH_VERSION | $(python3 --version 2>&1) | $(go version 2>&1) | $(uname -srm) | $(curl --version 2>&1 | head -1)"

case "$(uname -s)" in Linux) OS=linux ;; Darwin) OS=darwin ;; *) fatal "unsupported host OS $(uname -s)" ;; esac
case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; arm64|aarch64) ARCH=arm64 ;; *) fatal "unsupported host arch $(uname -m)" ;; esac

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# release TAG VERSION: build lmon stamped with VERSION, archive it like GoReleaser does
release() {
  local ver=$2 d="$WORK/rel/$1" b="$WORK/build-$2"
  mkdir -p "$d" "$b"
  local out
  out=$(cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "-X main.version=$ver -X main.commit=fake123" -o "$b/lmon" . 2>&1) || fatal "go build failed: $out"
  cp "$ROOT/LICENSE" "$b/"
  local a="lmon_${ver}_${OS}_${ARCH}.tar.gz"
  tar -czf "$d/$a" -C "$b" lmon LICENSE
  printf '%s  %s\n' "$(sha "$d/$a")" "$a" > "$d/checksums.txt"
}

echo "building fake releases..."
release v9.9.9 9.9.9
release v0.0.1 0.0.1

# a release whose archive was altered after its checksum was computed
mkdir -p "$WORK/rel/v6.6.6"
cp "$WORK/rel/v9.9.9/lmon_9.9.9_${OS}_${ARCH}.tar.gz" "$WORK/rel/v6.6.6/lmon_6.6.6_${OS}_${ARCH}.tar.gz"
printf '%s  %s\n' "$(sha "$WORK/rel/v6.6.6/lmon_6.6.6_${OS}_${ARCH}.tar.gz")" "lmon_6.6.6_${OS}_${ARCH}.tar.gz" > "$WORK/rel/v6.6.6/checksums.txt"
printf 'tampered' >> "$WORK/rel/v6.6.6/lmon_6.6.6_${OS}_${ARCH}.tar.gz"
# a release whose checksums.txt does not list the archive
mkdir -p "$WORK/rel/v5.5.5"
cp "$WORK/rel/v9.9.9/lmon_9.9.9_${OS}_${ARCH}.tar.gz" "$WORK/rel/v5.5.5/lmon_5.5.5_${OS}_${ARCH}.tar.gz"
echo "0000  some_other_file.tar.gz" > "$WORK/rel/v5.5.5/checksums.txt"

python3 "$ROOT/scripts/fake-release-server.py" "$WORK/rel" "$WORK/port" 2>"$WORK/server.err" &
SRV=$!
for _ in $(seq 1 100); do [ -s "$WORK/port" ] && break; sleep 0.1; done
[ -s "$WORK/port" ] || fatal "fake server did not start within 10s: $(cat "$WORK/server.err" 2>&1)"
BASE="http://127.0.0.1:$(cat "$WORK/port")"
mkdir -p "$WORK/home"

# run_install DIR [ENV=VAL ...]: runs install.sh into DIR; sets OUT and RC
run_install() {
  local dir=$1; shift
  OUT=$(env HOME="$WORK/home" LMON_BASE_URL="$BASE" LMON_INSTALL_DIR="$dir" "$@" sh "$ROOT/install.sh" 2>&1); RC=$?
}
version_of() { "$1/lmon" version 2>/dev/null; }

echo "install:"
D="$WORK/t1"
run_install "$D"
if [ $RC -eq 0 ] && [ "$(version_of "$D")" = "lmon 9.9.9 (fake123)" ]; then ok "latest release installs and runs"; else bad "latest release installs and runs" "rc=$RC $OUT"; fi
[ -x "$D/lmon" ] && ok "binary is executable" || bad "binary is executable"

D="$WORK/t2"
run_install "$D" LMON_VERSION=v0.0.1
[ "$(version_of "$D")" = "lmon 0.0.1 (fake123)" ] && ok "LMON_VERSION=v0.0.1 pins the version" || bad "pin with v prefix" "$OUT"
D="$WORK/t2b"
run_install "$D" LMON_VERSION=0.0.1
[ "$(version_of "$D")" = "lmon 0.0.1 (fake123)" ] && ok "LMON_VERSION=0.0.1 (no v) works too" || bad "pin without v prefix" "$OUT"

run_install "$WORK/t2"   # upgrade over the 0.0.1 install
if [ "$(version_of "$WORK/t2")" = "lmon 9.9.9 (fake123)" ] && [ "$(ls -A "$WORK/t2")" = "lmon" ]; then ok "upgrade replaces the binary and leaves no temp files"; else bad "upgrade" "$(ls -A "$WORK/t2") $OUT"; fi

D="$WORK/deep/er/dir"
run_install "$D"
[ -x "$D/lmon" ] && ok "creates a missing install directory" || bad "creates install dir" "$OUT"

OUT=$(env HOME="$WORK/home" LMON_BASE_URL="$BASE" sh "$ROOT/install.sh" 2>&1); RC=$?
[ -x "$WORK/home/.local/bin/lmon" ] && ok "defaults to \$HOME/.local/bin" || bad "default dir" "$OUT"

echo "PATH hint:"
D="$WORK/t3"
run_install "$D" PATH="/usr/bin:/bin:$(dirname "$(command -v curl)"):$(dirname "$(command -v go)")"
echo "$OUT" | grep -q "not on your PATH" && ok "warns when the install dir is not on PATH" || bad "PATH warning" "$OUT"
D="$WORK/t3b"
run_install "$D" PATH="$D:/usr/bin:/bin:$(dirname "$(command -v curl)"):$(dirname "$(command -v go)")"
echo "$OUT" | grep -q "not on your PATH" && bad "no warning expected when dir is on PATH" "$OUT" || ok "no warning when the install dir is on PATH"

echo "refuses bad downloads:"
D="$WORK/t4"; mkdir -p "$D"; echo OLD > "$D/lmon"
run_install "$D" LMON_VERSION=v6.6.6
if [ $RC -ne 0 ] && echo "$OUT" | grep -q "checksum mismatch" && [ "$(cat "$D/lmon")" = OLD ] && [ "$(ls -A "$D")" = "lmon" ]; then ok "tampered archive: fails, existing binary untouched, nothing left behind"; else bad "tampered archive" "rc=$RC $(ls -A "$D") $OUT"; fi
D="$WORK/t5"
run_install "$D" LMON_VERSION=v5.5.5
if [ $RC -ne 0 ] && echo "$OUT" | grep -q "not listed in checksums.txt" && [ ! -e "$D/lmon" ]; then ok "archive missing from checksums.txt: fails"; else bad "unlisted archive" "rc=$RC $OUT"; fi
D="$WORK/t6"
run_install "$D" LMON_VERSION=v1.2.3
if [ $RC -ne 0 ] && echo "$OUT" | grep -q "download failed" && [ ! -e "$D/lmon" ]; then ok "nonexistent release: fails with a clear message"; else bad "missing release" "rc=$RC $OUT"; fi
touch "$WORK/.no-latest-marker"; touch "$WORK/rel/.no-latest"
D="$WORK/t7"
run_install "$D"
if [ $RC -ne 0 ] && echo "$OUT" | grep -q "published release"; then ok "no release published yet: says so"; else bad "no release yet" "rc=$RC $OUT"; fi
rm -f "$WORK/rel/.no-latest"
D="$WORK/t8"; mkdir -p "$WORK/afile"; : > "$WORK/afile/x"
run_install "$WORK/afile/x/sub"
[ $RC -ne 0 ] && echo "$OUT" | grep -q "LMON_INSTALL_DIR" && ok "unwritable install dir: suggests LMON_INSTALL_DIR" || bad "unwritable dir" "rc=$RC $OUT"

echo "platform detection (dry run, uname/sysctl shimmed):"
SHIM="$WORK/shim"; mkdir -p "$SHIM"
# detect OS MACHINE [PROC_TRANSLATED] -> prints the asset name the script would fetch
detect() {
  cat > "$SHIM/uname" <<SH
#!/bin/sh
case "\$1" in -s) echo "$1" ;; -m) echo "$2" ;; esac
SH
  cat > "$SHIM/sysctl" <<SH
#!/bin/sh
[ -n "${3:-}" ] && echo "$3" || exit 1
SH
  chmod +x "$SHIM/uname" "$SHIM/sysctl"
  env PATH="$SHIM:$PATH" HOME="$WORK/home" LMON_BASE_URL="$BASE" LMON_VERSION=v1.0.0 LMON_DRY_RUN=1 sh "$ROOT/install.sh" 2>&1
}
expect_asset() { # OS MACHINE TRANSLATED WANT
  local out; out=$(detect "$1" "$2" "$3")
  echo "$out" | grep -q "lmon_1.0.0_$4.tar.gz" && ok "$1 $2${3:+ (Rosetta=$3)} -> $4" || bad "$1 $2 -> $4" "$out"
}
expect_asset Linux  x86_64  "" linux_amd64
expect_asset Linux  aarch64 "" linux_arm64
expect_asset Linux  arm64   "" linux_arm64
expect_asset Darwin arm64   "" darwin_arm64
expect_asset Darwin x86_64  "" darwin_amd64
expect_asset Darwin x86_64  1  darwin_arm64   # x86_64 shell on an Apple Silicon Mac
expect_asset Darwin x86_64  0  darwin_amd64   # a genuine Intel Mac
for spec in "FreeBSD x86_64" "Linux mips" "MINGW64_NT-10.0 x86_64" "Linux riscv64"; do
  set -- $spec
  out=$(detect "$1" "$2" ""); 
  if echo "$out" | grep -q "unsupported\|On Windows"; then ok "$1 $2 is rejected with guidance"; else bad "$1 $2 rejected" "$out"; fi
done
detect MINGW64_NT x86_64 "" | grep -q "go install" && ok "Windows message points to the zip and go install" || bad "windows message"

echo "interrupted download (curl | sh cut off mid-way):"
SIZE=$(wc -c < "$ROOT/install.sh")
LIMIT=$((SIZE - 10))   # the last line, 'main "$@"', is what starts everything
cut_ok=1; n=0
for len in $(seq 1 83 "$LIMIT") "$LIMIT"; do
  D="$WORK/cut$len"
  head -c "$len" "$ROOT/install.sh" | env HOME="$WORK/home" LMON_BASE_URL="$BASE" LMON_INSTALL_DIR="$D" sh >/dev/null 2>&1
  n=$((n + 1))
  if [ -e "$D/lmon" ] || [ -d "$D" ]; then cut_ok=0; bad "script cut at $len bytes still did something"; fi
done
[ $cut_ok -eq 1 ] && ok "$n different truncation points: none installed anything"

echo
echo "passed: $pass  failed: $fail"
[ $fail -eq 0 ]
