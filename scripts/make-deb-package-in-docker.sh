#!/bin/bash
set -euxo pipefail 
rm -rf build
docker rm -f telegraf-pkg-builder
CI_VERSION=$(cat Makefile  | grep telegraf-ci: | head -n1 | cut -d ':' -f2 | awk '{print $1}')
docker pull quay.io/influxdb/telegraf-ci:$CI_VERSION
cat <<EOF | docker run --name telegraf-pkg-builder --mount type=bind,source=$PWD,target=/go/src/github.com/influxdata/telegraf -i quay.io/influxdb/telegraf-ci:$CI_VERSION /bin/bash
set -euxo pipefail 
cd /go/src/github.com/influxdata/telegraf
make -j$(nproc) deps
make -j$(nproc) package include_packages="amd64.deb"
EOF
docker cp telegraf-pkg-builder:/go/src/github.com/influxdata/telegraf/build/dist build
docker rm telegraf-pkg-builder
scp build/dist/* vultr:/var/www/html/files/
