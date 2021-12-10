rm -rf dist
docker rm -f telegraf-pkg-builder
docker pull quay.io/influxdb/telegraf-ci:1.17.3
cat <<EOF | docker run --name telegraf-pkg-builder --mount type=bind,source=$PWD,target=/go/src/github.com/influxdata/telegraf -i quay.io/influxdb/telegraf-ci:1.17.3 /bin/bash
cd /go/src/github.com/influxdata/telegraf
make -j$(nproc) deps
make -j$(nproc) package include_packages="amd64.deb"
EOF
docker cp telegraf-pkg-builder:/go/src/github.com/influxdata/telegraf/build/dist build