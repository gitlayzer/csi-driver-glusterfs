//go:build deps

// This file is only compiled with `-tags deps` and is never part of the shipped
// binary. It pins the complete, approved dependency set of the driver in
// go.mod / go.sum from the first commit on:
//
//   - github.com/container-storage-interface/spec  CSI protobuf + gRPC stubs
//   - google.golang.org/grpc                       gRPC server (T-04)
//   - k8s.io/klog/v2                               structured logging
//   - github.com/kubernetes-csi/csi-lib-utils      protosanitizer (secret-safe request logs)
//
// The first three are consumed by the gRPC server added in task T-04; importing
// them here keeps `go mod tidy` from pruning them in the meantime. Verify with:
//
//	go build -tags deps ./...
package main

import (
	_ "github.com/container-storage-interface/spec/lib/go/csi"
	_ "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	_ "google.golang.org/grpc"
	_ "k8s.io/klog/v2"
)
