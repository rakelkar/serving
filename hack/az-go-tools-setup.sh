#!/bin/bash
set -e
depVersion=v0.5.1
echo "Installing go dep version ${depVersion} to $GOBIN"
# Install go dependency manager
curl https://raw.githubusercontent.com/golang/dep/master/install.sh | INSTALL_DIRECTORY=$GOBIN DEP_RELEASE_TAG=${depVersion} sh -

echo "Install ko"
# from https://github.com/google/ko
go get github.com/google/ko/cmd/ko

echo "Install kustomize"
opsys=linux
curl -s https://api.github.com/repos/kubernetes-sigs/kustomize/releases/tags/v3.0.2 |\
  grep browser_download |\
  grep $opsys |\
  cut -d '"' -f 4 |\
  xargs curl -O -L
mv kustomize_*_${opsys}_amd64 kustomize
chmod u+x kustomize
echo "Moving kustomize from  $PWD to $GOBIN"
mv -f kustomize $GOBIN