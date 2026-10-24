#!/usr/bin/env bash
# Commit every change in this repository and push it to github.com/izhangfly/NervusOS-Cambridge, the one
# repository NervusOS work goes to. Nothing is committed unless the staged content passes secret-scan.py.
#
#   scripts/push.sh "Subject line" ["Body"]   commit with this subject (and body)
#   scripts/push.sh                           commit with a subject naming the changed files
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Commits are credited to the owner only (their rule): no co-author trailer unless one is asked for.
TRAILER="${NERVUS_COMMIT_TRAILER:-}"
cd "$REPO" || exit 1

lock="$REPO/.git/nervus-push.lock"
if ! mkdir "$lock" 2>/dev/null; then
	if [ -n "$(find "$lock" -maxdepth 0 -mmin +10 2>/dev/null)" ]; then
		rm -rf "$lock"; mkdir "$lock" || exit 0
	else
		echo "push: another push is running" >&2; exit 0
	fi
fi
trap 'rmdir "$lock" 2>/dev/null' EXIT

case "$(git remote get-url origin 2>/dev/null)" in
	*github.com*izhangfly/NervusOS-Cambridge*) ;;
	*) echo "push: origin is not izhangfly/NervusOS-Cambridge; refusing" >&2; exit 3 ;;
esac

push() {
	git push -q origin HEAD:main 2>/dev/null && return 0
	# The other machine pushed first: replay on top of it, then push.
	git pull -q --rebase origin main && git push -q origin HEAD:main
}

git add -A
if git diff --cached --quiet; then
	[ -n "$(git log origin/main..HEAD --oneline 2>/dev/null)" ] && push
	exit $?
fi
if ! python3 "$REPO/scripts/secret-scan.py"; then
	git reset -q
	echo "push: the secret scan failed; nothing was committed" >&2
	exit 2
fi
subject="${1:-}"
if [ -z "$subject" ]; then
	n="$(git diff --cached --name-only | wc -l | tr -d ' ')"
	first="$(git diff --cached --name-only | head -3 | while read -r f; do basename "$f"; done | paste -sd ',' - | sed 's/,/, /g')"
	subject="Update $first"
	[ "$n" -gt 3 ] && subject="$subject and $((n - 3)) more"
fi
if [ -n "${2:-}" ]; then
	git commit -q -m "$subject" -m "$2" ${TRAILER:+-m "$TRAILER"} || exit 1
else
	git commit -q -m "$subject" ${TRAILER:+-m "$TRAILER"} || exit 1
fi
push && echo "push: $(git rev-parse --short HEAD) $subject"
