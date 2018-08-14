#!/bin/bash

set -ex

# make depend fails if run as gpadmin with a dep ensure git-remote-https signal 11 failure
GOPATH="$PWD/go" PATH="$PWD/go/bin:$PATH" make -C go/src/github.com/greenplum-db/gpupgrade depend

source gpdb_src/concourse/scripts/common.bash
time install_gpdb
time ./gpdb_src/concourse/scripts/setup_gpadmin_user.bash "centos"
time make_cluster

time chown -R gpadmin:gpadmin go

su gpadmin <<'EOF'
export GOPATH=$PWD/go
export PATH=$GOPATH/bin:$PATH
source /usr/local/greenplum-db-devel/greenplum_path.sh
source gpdb_src/gpAux/gpdemo/gpdemo-env.sh

cd $GOPATH/src/github.com/greenplum-db/gpupgrade
make
make check

# Note that installcheck is currently destructive.
make install
make installcheck
EOF

