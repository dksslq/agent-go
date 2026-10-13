#!/usr/bin/env bash
set -euo pipefail

package_name=$(dpkg-parsechangelog -S Source)
package_version=$(dpkg-parsechangelog -S Version)
if [ "$#" -gt 0 ]; then
  package_version="${1#v}-1"
fi
package_dir=$(mktemp -d /tmp/agent-go-package.XXXXXX)
trap 'rm -rf "$package_dir"' EXIT
git archive HEAD --prefix "$package_name/" | tar -x -C "$package_dir"
sed -i "1s/([^)]*)/($package_version)/" "$package_dir/$package_name/debian/changelog"
set +e
# dh-golang resolves standard-library build dependencies through dpkg. Use the
# distribution-owned SDK even when setup-go put a separate SDK first in PATH.
(cd "$package_dir/$package_name" && env -u GOROOT PATH="/usr/bin:/bin:$PATH" dpkg-buildpackage -us -uc -b) > "$package_dir/build.log" 2>&1
package_status=$?
set -e
cat "$package_dir/build.log"
if [ "$package_status" -ne 0 ]; then
  package_error=$(tail -80 "$package_dir/build.log")
  package_error=${package_error//%/%25}
  package_error=${package_error//$'\r'/%0D}
  package_error=${package_error//$'\n'/%0A}
  printf '::error title=Debian build failed::%s\n' "$package_error"
  exit "$package_status"
fi
mkdir -p dist
cp "$package_dir/${package_name}_${package_version}_amd64.deb" dist/
