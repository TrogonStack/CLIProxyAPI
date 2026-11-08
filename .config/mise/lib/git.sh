# shellcheck shell=bash
# git_push_as <token> <git push args...> pushes with the given token instead of
# the job's GITHUB_TOKEN. The token is read from the environment by the
# credential helper, so it never appears on a command line. actions/checkout
# sends GITHUB_TOKEN as an extra header, which would win over the helper, so
# that header is reset for this push.
# shellcheck disable=SC2016 # expanded by the credential helper, not here
git_push_as() {
  PUSH_TOKEN="$1" git -c http.https://github.com/.extraheader= -c credential.helper= \
    -c 'credential.helper=!f() { echo username=x-access-token; echo "password=${PUSH_TOKEN}"; }; f' \
    push "${@:2}"
}
