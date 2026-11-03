#!/bin/sh
# $0 is the per-test symlink on PATH, so logs stay inside that test's bin dir.
printf '%s\n' "$*" >> "${0%/*}/dolt.log"
case "$*" in
  *"SELECT name, url FROM dolt_remotes"*)
    printf 'name,url\nalpha,file:///data/remotes/alpha\ninternal,file:///data/remotes/internal\npublic,https://public.example.invalid/repo\n'
    ;;
esac
exit 0
