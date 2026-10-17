#!/usr/bin/env bash
# Reproducible resource/retrieval benchmark using synthetic TXT books.
# Reports actual measured numbers only. Usage: ./scripts/benchmark.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO/bin/boaky"
ROOT="${ROOT:-/tmp/boaky-bench}"
BOOKS="$ROOT/books"
export BOAKY_BOOKS_DIR="$BOOKS"
export BOAKY_DB_PATH="$ROOT/data/index/boaky.db"
export BOAKY_META_DIR="$ROOT/data/metadata"
rm -rf "$ROOT"
mkdir -p "$BOOKS" "$ROOT/data/index" "$ROOT/data/metadata"

[ -x "$BIN" ] || (cd "$REPO" && go build -o "$BIN" ./cmd/boaky)

echo "=== generating synthetic library ==="
python3 - "$BOOKS" <<'EOF'
import random, sys, os
books_dir = sys.argv[1]
random.seed(42)
words = ['memory', 'identity', 'river', 'mirror', 'dream', 'shadow', 'bridge',
         'self', 'time', 'silence', 'forget', 'remember', 'chapter', 'narrator',
         'voice', 'night', 'dust', 'clock']
def para(n):
    return ' '.join(random.choice(words) for _ in range(n))
counts = 12 * [(15, 8)] + 6 * [(40, 25)] + 1 * [(200, 120)]
for bi, (ch, pp) in enumerate(counts):
    with open(os.path.join(books_dir, f'synth{bi:03d}.txt'), 'w') as f:
        f.write("The Project Gutenberg eBook of Synthetic Volume\n\n"
                "Title: Synthetic Volume\n\nAuthor: Test Person\n\n"
                "*** START OF THE PROJECT GUTENBERG EBOOK SYNTHETIC VOLUME ***\n\n")
        for c in range(ch):
            f.write(f"Chapter {c + 1}\n\n")
            for p in range(pp):
                f.write(para(random.randint(30, 120)) + "\n\n")
        f.write("*** END OF THE PROJECT GUTENBERG EBOOK SYNTHETIC VOLUME ***\n")
EOF
du -sh "$BOOKS" | awk '{print "library size:", $1}'

timed() { # label; command follows
  local label="$1"; shift
  local t
  t=$( { time -p "$@" >/dev/null; } 2>&1 | awk '/^real/{print $2}' )
  echo "$label: ${t}s"
}

echo
echo "=== indexing ==="
timed "cold index"     "$BIN" index
timed "skip (unchanged)" "$BIN" index
timed "force rebuild"  "$BIN" index --force

echo
echo "=== search latency (5 queries x 3 runs) ==="
for q in "memory and identity" "narrator forgets" "mirror and self" "night and silence" "river of time"; do
  for i in 1 2 3; do
    t=$( { time -p "$BIN" search "$q" --limit 10 >/dev/null; } 2>&1 | awk '/^real/{print $2}' )
    printf "  %-24s run%d %ss\n" "$q" "$i" "$t"
  done
done

echo
echo "=== database size ==="
ls -lh "$ROOT/data/index/boaky.db" | awk '{print "  boaky.db:", $5}'
ls "$ROOT/data/index/" | sed 's/^/  /'

echo
echo "=== peak memory (force rebuild) ==="
/usr/bin/time -l "$BIN" index --force 2>&1 >/dev/null | awk '/maximum resident/{print "peak RSS:", $1, "bytes"}'

echo
echo "=== idle server memory ==="
"$BIN" serve-http >/dev/null 2>&1 &
PID=$!
sleep 2
RSS=$(ps -o rss= -p $PID)
echo "  idle server RSS: ${RSS} KB"
kill $PID 2>/dev/null
