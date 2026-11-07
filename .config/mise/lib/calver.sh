# shellcheck shell=bash
# decide <digest> <today YYYY.MDD> reads "<tag> <digest>" lines of the
# existing version tags on stdin. It prints "released <tag>" when a
# tag already points at <digest>, otherwise "next <YYYY.MDD.N>".
decide() {
  local digest="$1" today="$2" tag tagdigest n=0
  while read -r tag tagdigest; do
    [ -n "$tag" ] || continue
    if [ "$tagdigest" = "$digest" ]; then
      echo "released $tag"
      return 0
    fi
    case "$tag" in "$today".*) n=$((n + 1)) ;; esac
  done
  echo "next $today.$n"
}
