############################################################################
# Copyright 2019 Intel Corporation. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
############################################################################

#!/bin/sh
set -e

export RTE_SDK=/opt/dpdk-18.11.2
export RTE_TARGET=x86_64-native-linuxapp-gcc

dpdk_name=dpdk-18.11.2
dpdk_url=http://fast.dpdk.org/rel/${dpdk_name}.tar.xz

export NES_SERVER_CONF=$(pwd)/nes.cfg
CONFIG_PATH=$NES_SERVER_CONF
NES_SERVER_CONF_DEFAULT_PATH="$PWD/nes.cfg"
export CMDLINE_CONF_PATH="$PWD/nes.cfg"

log()
{
    green='\033[0;32m'
    reset='\e[0m'
    echo -e "* ${green}$1${reset}"
}

# Download the source package given as argument, extract it
# and chdir to the root of the extracted tree
get_source()
{
    local url="$1"
    local name

    # 1. Download
    log "downloading $url"
    cd $setup_dir
    if ! wget -cq "$url"; then
        log "failed downloading $url, bailing out"
        exit 1
    fi

    # 2. Extract
    name=$(basename $url .tar.xz)
    log "extracting $name"
    mkdir -p "$name"
    cd "$name"
    tar --strip 1 -xf "../$(basename $url)"
}

if ! test -d /opt/$dpdk_name; then
    (
    get_source "$dpdk_url"
    log "Compiling DPDK (this will take a while). Output in dpdk-install.log"
    make install T=x86_64-native-linuxapp-gcc > ../dpdk-install.log 2>&1
    cd .. && mv $(basename $OLDPWD) /opt/
    )
fi

mkdir -p /mnt/huge
mount -t hugetlbfs none /mnt/huge
rm -rf daemon/build
make clean && make
export NES_SERVER_CONF=${CMDLINE_CONF_PATH}
./daemon/build/nes-daemon-unit-tests -c 0xe -n 4  --huge-dir /mnt/huge --file-prefix=tests-1 --socket-mem 2048,0

rm -rf /mnt/huge/*
umount /mnt/huge/
rm -rf coverage
mkdir -p coverage

cd coverage
echo "Creating coverage report"
COVERAGE_INFO=coverage.info
BUILD_PATH=../daemon/build/
lcov --capture --directory ${BUILD_PATH} --output-file ${COVERAGE_INFO} --rc lcov_branch_coverage=1 2>&1 > /dev/null
lcov --extract ${COVERAGE_INFO} "*/daemon/*" -o ${COVERAGE_INFO} --rc lcov_branch_coverage=1 2>&1 > /dev/null
lcov --remove ${COVERAGE_INFO} "*/rnis/*" --remove ${COVERAGE_INFO} "*tests/daemon/*" -o ${COVERAGE_INFO} --rc lcov_branch_coverage=1 2>&1 >> summary.txt
genhtml ${COVERAGE_INFO} --output-directory --rc lcov_branch_coverage=1 out 2>&1 > /dev/null
