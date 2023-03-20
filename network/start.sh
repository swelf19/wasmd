#!/bin/bash

NEUTROND_BINARY=neutrond
GAIAD_BINARY=gaiad
CHAIN_DIR=./data
CHAINID_1=test-1
CHAINID_2=test-2
GRPCPORT_1=8090
GRPCPORT_2=9090
GRPCWEB_1=8091
GRPCWEB_2=9091

echo "Starting $CHAINID_1 in $CHAIN_DIR..."
echo "Creating log file at $CHAIN_DIR/$CHAINID_1.log"
$NEUTROND_BINARY start --log_level debug --log_format json --home $CHAIN_DIR/$CHAINID_1 --pruning=nothing --grpc.address="0.0.0.0:$GRPCPORT_1" --grpc-web.address="0.0.0.0:$GRPCWEB_1" --trace > $CHAIN_DIR/$CHAINID_1.log 2>&1 &

echo "Starting gaiad chain $CHAINID_2 in $CHAIN_DIR..."
echo "Creating log file at $CHAIN_DIR/$CHAINID_2.log"
$GAIAD_BINARY start --log_level debug --log_format json --home $CHAIN_DIR/$CHAINID_2 --pruning=nothing --grpc.address="0.0.0.0:$GRPCPORT_2" --grpc-web.address="0.0.0.0:$GRPCWEB_2" > $CHAIN_DIR/$CHAINID_2.log 2>&1 &