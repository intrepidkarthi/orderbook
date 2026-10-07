#!/usr/bin/env bash
# Prepares a pinned CppTrader checkout for CMakeLists.txt. CI only (ubuntu-24.04):
# it clones third-party code, so it is never run on a workstation.
#
#   bench/xeng/cpptrader/fetch.sh <dir>
#
# gil clones every .gitlinks module at a branch head, so after `gil update` each
# module is checked out at the commit its branch held when CppTrader was pinned
# (2026-09-09, the commit upstream's own CI built green). The paths are the real
# clones gil makes; the other module paths are symlinks to them.
set -euo pipefail

dir=${1:?usage: fetch.sh <dir>}

CPPTRADER_SHA=39421f50ae673be34235e24eac95827220763652
GIL_VERSION=1.25.0.0

pins=(
  "modules/Catch2                          15db9be53067d66ecd11a7ddd3097d412047130a"
  "modules/cpp-optparse                    b90dd73f1d33fb36d782107c233e8f46e2ee40c3"
  "modules/CppBenchmark                    ff29fd3e5380bbf5c3aa99b37253ddd7b4403a26"
  "modules/CppCommon                       9eba9ba6034a2b408a50fb84593adb8730e5e954"
  "build                                   367c2b5aa4f8a5a69713f1b2f101205bafc00d82"
  "cmake                                   24ed6954308bf6614f7e0c87134d2f1695e02b75"
  "modules/CppBenchmark/modules/HdrHistogram 55525ee1a1b9c27e053345d57e2e94a4c2fbf747"
  "modules/CppBenchmark/modules/zlib       da607da739fa6047df13e66a2af6b8bec7c2a498"
  "modules/CppCommon/modules/fmt           44f2c7ae018adca38ad266df471dc66b6a10ea6c"
  "modules/CppCommon/modules/vld           f9e61dcbd3e260c4a0b1c574499d79a6a987c880"
)

git init -q "$dir"
git -C "$dir" fetch -q --depth 1 https://github.com/chronoxor/CppTrader.git "$CPPTRADER_SHA"
git -C "$dir" checkout -q FETCH_HEAD

pip3 install --user --break-system-packages "gil==$GIL_VERSION"
(cd "$dir" && "$HOME/.local/bin/gil" update)

for p in "${pins[@]}"; do
  read -r path sha <<<"$p"
  git -C "$dir/$path" checkout -q --detach "$sha"
  echo "pinned $path $(git -C "$dir/$path" rev-parse HEAD)"
done
