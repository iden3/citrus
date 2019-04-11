#!/bin/sh

apt-get install -y wget
# Install python3.6
tmpdir="$(mktemp -d --suff citrus)"
cd ${tmpdir}
PYURL="https://github.com/chriskuehl/python3.6-debian-stretch/releases/download"
PYVER1="3.6.3-1-deb9u1"
PYVER2="3.6.3-1.deb9u1"
wget ${PYURL}/v${PYVER1}/python3.6_${PYVER2}_amd64.deb
wget ${PYURL}/v${PYVER1}/python3.6-minimal_${PYVER2}_amd64.deb
wget ${PYURL}/v${PYVER1}/python3.6-dev_${PYVER2}_amd64.deb
wget ${PYURL}/v${PYVER1}/libpython3.6_${PYVER2}_amd64.deb
wget ${PYURL}/v${PYVER1}/libpython3.6-minimal_${PYVER2}_amd64.deb
wget ${PYURL}/v${PYVER1}/libpython3.6-stdlib_${PYVER2}_amd64.deb
wget ${PYURL}/v${PYVER1}/libpython3.6-dev_${PYVER2}_amd64.deb
dpkg -i *.deb || true
apt-get -f install -y
cd /
rm -r "${tmpdir}"
