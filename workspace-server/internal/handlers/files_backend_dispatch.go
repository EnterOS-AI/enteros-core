package handlers

import "strings"

// isEC2InstanceID reports whether a workspace's persisted instance_id refers
// to a real AWS EC2 box — i.e. the workspace is on the AWS (SaaS) backend and
// its Files API traffic must ride the EC2-Instance-Connect (EIC) SSH tunnel.
// Every AWS EC2 instance id has the form "i-<hex>" ("i-" + 8 legacy or 17
// current hex chars); the "i-" prefix is an AWS-assigned invariant, which is
// exactly why it is a safe, cheap backend discriminator here.
//
// WHY THIS GATE EXISTS (molecules-server parity):
//
// The workspaces.instance_id column was designed to be NULL for local-Docker
// workspaces (migration 038: "local-Docker workspaces never populate this
// column"). But the CP local-docker provisioner returns the workspace's
// CONTAINER NAME as its WorkspaceInstance.InstanceID (local_docker_workspace.go
// → `return &WorkspaceInstance{InstanceID: name}`), and provisionWorkspaceCP
// persists that value verbatim into instance_id. So a molecules-server
// (local-docker) workspace ends up with instance_id = "mol-ws-<slug>-<hex>"
// (a container name) — non-empty, and NOT an EC2 id (verified against the live
// self-host DB: instance_id="mol-ws-test5-a9f3044fa3f2", compute.provider NULL).
//
// The Files API verbs (List/Read/Write/Delete + bulk Replace) used to dispatch
// on `instanceID != ""`, which sent those local-docker workspaces down the
// EC2-only EIC path. EIC is AWS-specific (aws ec2-instance-connect
// send-ssh-public-key against a real EC2 id), so it failed with
// "eic tunnel setup: send-ssh-public-key: ... context deadline exceeded" and
// the handler returned HTTP 500 — e.g. the canvas Config tab / template-delivery
// e2e could never read config.yaml on a molecules-server tenant.
//
// Routing by the instance_id SHAPE fixes this without depending on
// compute->>'provider' (which is empty/NULL for molecules-server workspaces):
// a real EC2 id ("i-...") keeps the AWS EIC path; anything else (a local-docker
// container name, or the empty string for external workspaces) falls through to
// the docker-exec path that mirrors how local_docker provisioning writes those
// same files.
func isEC2InstanceID(instanceID string) bool {
	// "i-" alone (no trailing chars) is never a real id nor a container name.
	return len(instanceID) > 2 && strings.HasPrefix(instanceID, "i-")
}
