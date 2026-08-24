---
title: from-hub-hosting-cluster-correct-metadata-and-resource-delivery
authors:
 - "@yiraeChristineKim"
reviewers: # Include a comment about what domain expertise a reviewer is expected to bring and what area of the enhancement you expect them to focus on.
 - "TBD, for hypershift-addon-operator / HCP proxy, please look at the new proxy endpoints (W1/W2/W4) and `removeHCFinalizer` follow-up"
 - "TBD, for product-cli/hcp CLI, please look at the from-hub client changes (create.go, client.go)"
approvers:
 - TBD
api-approvers: # HCP proxy (hcp.ocm.io/v1alpha1) gains new read endpoints and an extended create payload; see API Extensions
 - TBD
creation-date: 2026-08-07
last-updated: 2026-08-07
status: provisional
tracking-link:
 - "https://redhat.atlassian.net/browse/ACM-37265" # HCP Proxy epic
see-also:
 - "/FROM-HUB-ISSUES.md"
 - "/FROM-HUB-CLIENT-DESIGN-ISSUES.md"
replaces:
superseded-by:
---

# from-hub Hosting-Cluster-Correct Metadata and Resource Delivery

## Summary

`hcp from-hub create` lets a user create a `HostedCluster` from the **hub**, without direct
kubeconfig access to the **hosting cluster** — the CLI renders manifests locally and POSTs
them through the HCP proxy (`hcp.ocm.io/v1alpha1`), which impersonates the caller onto the
hosting cluster. Because `from-hub create` reuses `product-cli`'s core create path
(`core.CreateCluster`, platform `Complete()`/validation helpers) unchanged, several of those
helpers still call `util.GetClient()`/`util.GetConfig()`, which resolve to the default
kubeconfig — the **hub**, not the hosting cluster. This enhancement adds the hosting-cluster
metadata the HCP proxy needs to expose (HyperShift Operator version, supported OCP versions)
and a generic non-secret object passthrough so that version checks, release-stream defaulting,
and Agent-platform Role/ConfigMap delivery are correct for the hosting cluster instead of
silently reading from, or silently dropping resources destined for, the hub.

## Motivation

`hcp from-hub` exists specifically so operators can manage `HostedCluster`s without a
hosting-cluster admin kubeconfig. Every place where reused core-CLI code still reaches for
`util.GetClient()`/`util.GetConfig()` quietly breaks that guarantee: it either validates
against the wrong cluster (the hub) or drops resources the hosting cluster needs, with no
error to signal the drop. Today, three of these gaps have documented client-side workarounds
that are blocked on new, minimal read/write endpoints on the HCP proxy; a fourth
(non-secret resource delivery) is fixed client-side but still incomplete because the proxy
doesn't yet apply the objects it receives.

### User Stories

* As an operator using `hcp from-hub create --version-check`, I want the CLI to compare its
 version against the HyperShift Operator running on the **hosting** cluster, so that the
 check catches a real compatibility mismatch instead of comparing against an unrelated hub
 operator version (or failing closed with no check at all).
* As an operator using `hcp from-hub create --release-stream 4-stable-multi` (no
 `--release-image`), I want the resolved OCP release to be filtered against the **hosting**
 cluster's supported versions, so that I don't get a release image the hosting cluster's
 HyperShift Operator cannot actually run.
* As an operator running `hcp from-hub create agent` without `--api-server-address`, I want a
 stable, hosting-cluster-correct default (`api.<name>.<base-domain>`) instead of a value
 derived from a hub node IP, so that the HostedCluster's published API server address is
 usable and doesn't change if a hub node is drained.
* As an operator running `hcp from-hub create agent` (or any platform whose
 `GenerateResources()` emits a `Role`, `ConfigMap`, or other non-secret object), I want that
 object to actually exist on the hosting cluster after create, so that the HostedCluster
 doesn't reference objects that were never created.

### Goals

* `--version-check` on `hcp from-hub create` validates against the hosting cluster's
 HyperShift Operator version, never the hub's.
* `--release-stream` defaulting/filtering on `hcp from-hub create` uses the hosting cluster's
 supported OCP versions, never the hub's.
* `hcp from-hub create agent` defaults `--api-server-address` from `--name`/`--base-domain`
 instead of a hub node lookup, with no proxy round trip required.
* Non-secret objects rendered by a platform's `GenerateResources()` (Agent's
 `capi-provider-role` `Role`, trust-bundle/proxy `ConfigMap`s, etc.) are created on the
 hosting cluster as part of `from-hub create`, not silently dropped.
* All of the above are delivered without requiring a hosting-cluster/ManagedCluster
 kubeconfig on the client — the HCP proxy + caller's hub identity remains the only trust
 path (see [Alternatives](#alternatives-not-implemented)).

### Non-Goals

* Replacing the HCP proxy architecture with direct hosting-cluster kubeconfig access.
* Fixing `from-hub`'s own (non-reused-code) bugs tracked in
 [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md) (edit/delete GET-decode
 mismatch, edit's patch `Content-Type`, `--namespace` dual binding, `delete aws` finalizer
 cleanup) — those are independent client-side bugs, not collisions from reusing core-CLI code,
 and are out of scope for this enhancement.
* Fixing issues that have no workaround defined yet (hosting-cluster existence/arch checks
 skipped in render mode) — those need their own design pass and are not included here.

## Proposal

Add a small, read-mostly "hosting cluster view" surface to the HCP proxy
(`hcp.ocm.io/v1alpha1`) that `hcp from-hub create` calls instead of `util.GetClient()`, plus a
generic non-secret passthrough field on the existing create payload. `from-hub create` is
changed to capture the user's intent for `--version-check`/`--release-stream`, force the
reused `core.CreateCluster` options so those helpers never call `util.GetClient()`, resolve
the same facts via the proxy when requested, and only then render + POST as it does today.

### Workflow Description

**operator** is the human (or automation) invoking `hcp from-hub create`, authenticated to the
**hub** only.

**HCP proxy** (`hypershift-addon-operator`, serving `hcp.ocm.io/v1alpha1`) is the component
that receives the hub-authenticated request, enforces `managedcluster:admin` +
impersonation, and talks to the **hosting** cluster on the operator's behalf.

1. The operator runs `hcp from-hub create <platform> --hub-kubeconfig <hub> --hosting-cluster
 <name> --base-domain <domain> [--version-check] [--release-stream <stream>] ...`.
2. `from-hub create` (`runViaProxy`) captures `wantVersionCheck := coreOpts.VersionCheck` and
 forces `coreOpts.VersionCheck = false` before calling `core.CreateCluster`, so core's own
 `validateVersion()` never runs against the hub (W1).
3. If `wantVersionCheck` was set, `from-hub create` calls the new proxy metadata endpoint for
 the hosting cluster's HyperShift Operator `serverVersion`, and compares it to the CLI's own
 revision, failing closed on mismatch or on proxy/version unavailability.
4. If `coreOpts.ReleaseImage` is unset and `coreOpts.ReleaseStream` is set, `from-hub create`
 calls the same proxy metadata endpoint for the hosting cluster's `supportedVersions`,
 resolves a concrete release image client-side, sets `coreOpts.ReleaseImage`, and clears
 `coreOpts.ReleaseStream` so core's `resolveReleaseImage()` never runs against the hub (W2).
5. For the Agent platform, if `--api-server-address` is unset, `from-hub create` defaults
 `agentOpts.APIServerAddress = "api.<name>.<base-domain>"` before calling `core.CreateCluster`,
 so Agent's `Complete()` never calls `GetAPIServerAddressByNode()` against the hub (W3). No
 proxy call is needed for this step.
6. `core.CreateCluster` renders manifests locally (as it already does for from-hub). While
 building the proxy `CreateRequest`, any rendered object whose `Kind` is not
 `HostedCluster`/`NodePool`/`Secret` (e.g. Agent's `capi-provider-role` `Role`, a
 `user-ca-bundle` `ConfigMap`) is appended to `CreateRequest.ExtraObjects` instead of being
 dropped (W4, client-side — already done).
7. `from-hub create` POSTs the `CreateRequest` (now including `ExtraObjects`) to the HCP
 proxy. The proxy creates each `ExtraObjects` entry on the hosting cluster, in order, before
 creating the `HostedCluster`/`NodePool`s (W4, server-side — new in this enhancement).

```mermaid
sequenceDiagram
 participant Op as operator
 participant CLI as hcp from-hub create
 participant Proxy as HCP proxy (hcp.ocm.io/v1alpha1)
 participant Hosting as hosting cluster

 Op->>CLI: hcp from-hub create agent --version-check --release-stream ...
 CLI->>CLI: coreOpts.VersionCheck = false (never check hub)
 CLI->>Proxy: GET hosting version/supportedVersions
 Proxy->>Hosting: read supported-versions ConfigMap
 Proxy-->>CLI: serverVersion, supportedVersions
 CLI->>CLI: compare version; resolve release image; render manifests
 CLI->>Proxy: POST CreateRequest {HostedCluster, NodePools, Secrets, ExtraObjects}
 Proxy->>Hosting: create ExtraObjects, then HostedCluster/NodePools/Secrets
 Proxy-->>CLI: 201 Created
```

#### Error handling / failure recovery

* If the proxy's version/supported-versions endpoint is unreachable or returns an error while
 `--version-check` or a `--release-stream`-without-`--release-image` was explicitly requested,
 `from-hub create` fails closed with a clear error — it never silently skips the check or
 falls back to a hub-derived value.
* If the proxy fails to apply one or more `ExtraObjects` entries, it must fail the whole create
 request (not partially create `HostedCluster`/`NodePool`s referencing objects that don't
 exist) and return an error the CLI surfaces to the operator.

### API Extensions

This enhancement extends the existing `hcp.ocm.io/v1alpha1` HCP proxy surface
(`hypershift-addon-operator`'s `pkg/manager/hcp_proxy.go`). It does not add a CRD, webhook, or
finalizer.

* New read endpoint, used by W1 and W2 (one endpoint serving both, to avoid two round trips):

 ```text
 GET /apis/hcp.ocm.io/v1alpha1/namespaces/{ns}/version?hostingCluster={name}
 ```

 ```json
 {
 "serverVersion": "<hosting-ho-git-sha>",
 "supportedVersions": ["4.16", "4.17", "4.18"]
 }
 ```

 Backed by the hosting cluster's `hypershift.openshift.io/supported-versions` ConfigMap in the
 `hypershift` namespace — the same source `validateVersion`/`LookupDefaultOCPVersion` already
 read via `util.GetClient()` today, just fetched through the proxy against the hosting cluster
 instead of the hub. Auth/RBAC matches other proxy routes (`managedcluster:admin` +
 impersonation). Tracked as [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227) (W1) and
 [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228) (W2), both children of the HCP
 Proxy epic [ACM-37265](https://redhat.atlassian.net/browse/ACM-37265).

* Extended create request contract, used by W4 (client-side already implemented in this repo):

 ```go
 type CreateRequest struct {
 HostedCluster *hypershiftv1beta1.HostedCluster `json:"hostedCluster"`
 NodePools []*hypershiftv1beta1.NodePool `json:"nodePools,omitempty"`
 Secrets []corev1.Secret `json:"secrets,omitempty"`

 // ExtraObjects holds any other non-secret object emitted by a platform's
 // GenerateResources() (e.g. Agent's capi-provider-role Role, the
 // user-ca-bundle ConfigMap from --additional-trust-bundle) that doesn't
 // have a dedicated field above. The server creates each one verbatim on
 // the hosting cluster, in the given order, before the HostedCluster.
 ExtraObjects []runtime.RawExtension `json:"extraObjects,omitempty"`
 }
 ```

 The proxy's `handleCreate` must decode and apply each `ExtraObjects` entry on the hosting
 spoke, in the same phase as `Secrets`, before creating the `HostedCluster`. Tracked as
 [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216).

This does not modify the behavior of resources owned by other parties; it only adds new
read-only proxy routes and a new optional field to an existing proxy-owned request type.

### Topology Considerations

#### Hypershift / Hosted Control Planes

This enhancement is entirely about `hcp from-hub`, a HyperShift-specific CLI flow for creating
`HostedCluster`s from the ACM hub via the HCP proxy that ships in
`hypershift-addon-operator`. It has no effect on `hcp create cluster` (direct kubeconfig
flow), which already talks to the correct (hosting/management) cluster via
`util.GetClient()`.

#### Standalone Clusters

Not applicable — `hcp from-hub` only exists in the HyperShift/ACM multi-cluster context.

#### Single-node Deployments or MicroShift

Not applicable.

#### OpenShift Kubernetes Engine

Not applicable — from-hub is an ACM/HyperShift-addon capability, not a core OCP/OKE feature.

### Implementation Details/Notes/Constraints

The concrete client-side changes are already specified as workarounds W1–W4 in
[FROM-HUB-ISSUES.md](FROM-HUB-ISSUES.md); this enhancement is the design record for finishing
the server-side (proxy) half and removing the interim "reject the flag" behavior once the
proxy endpoints exist. Summary of what remains per workaround, unresolved today:

| Workaround | Issue(s) | Client-side | Server-side (proxy) | Tracking |
|---|---|---|---|---|
| W1 — `--version-check` via proxy | [Issue 1](FROM-HUB-ISSUES.md#1---version-check-compares-against-hub-hypershift-operator-high) | Interim: flag rejected outright. Full check pending endpoint. | Not implemented | [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227) |
| W2 — `--release-stream` via proxy | [Issue 2](FROM-HUB-ISSUES.md#2---release-stream-defaults-using-hub-supported-versions-high) | Not implemented | Not implemented | [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228) |
| W3 — Agent API server address default | [Issue 3](FROM-HUB-ISSUES.md#3-agent-api-server-address-taken-from-hub-nodes-high) | `--base-domain` required (done); `APIServerAddress` defaulting logic still open | N/A (no proxy call needed) | — |
| W4 — `ExtraObjects` passthrough | [Issues 4](FROM-HUB-ISSUES.md#4-agent-role-rendered-then-dropped-before-reaching-the-hosting-cluster-high) & [5](FROM-HUB-ISSUES.md#5-configmaps-dropped-from-create-payload-high) | Done | Not implemented | [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216) |

W1 and W2 share one proxy GET (`serverVersion` + `supportedVersions` in a single response) to
avoid two round trips per `from-hub create` invocation. W3 needs no proxy call at all — it is
purely derived from flags already available to the CLI (`--name`, `--base-domain`). W4 is a
single generic mechanism (rather than one typed field per object `Kind`) so that a future
platform emitting a new `ConfigMap`/`RoleBinding`/etc. doesn't silently reintroduce the drop
bug — see [FROM-HUB-ISSUES.md, W4](FROM-HUB-ISSUES.md#w4-generic-passthrough-for-non-secret-objects-issues-4-and-5)
for the full rationale and sketch.

### Risks and Mitigations

* **New proxy endpoints widen the HCP proxy's read surface.** Mitigated by scoping the new GET
 to the same ConfigMap the hosting cluster's own `hypershift` namespace already exposes to
 admins, and reusing the existing `managedcluster:admin` + impersonation checks — no new RBAC
 model is introduced.
* **`ExtraObjects` lets the proxy create arbitrary decoded objects on the hosting cluster.**
 Mitigated by keeping the accepted `Kind`s to what known platforms' `GenerateResources()`
 actually emit (Role, ConfigMap, RBAC, core types) rather than accepting fully arbitrary
 `GroupVersionKind`s, and requiring the whole create request to fail if any `ExtraObjects`
 entry cannot be applied (no partial application).
* **Fail-closed behavior for `--version-check`/`--release-stream` may surprise users** who are
 used to it silently working (against the wrong cluster) today. Mitigated by clear, explicit
 error messages instead of silent success, and by documenting the interim "flag rejected"
 behavior in `docs/content/how-to/common/from-hub-cli.md`.
* Security review for the new proxy endpoints and the `ExtraObjects` apply path should be done
 by the `hypershift-addon-operator` maintainers before the proxy-side work merges.

### Drawbacks

* Adds two new proxy endpoints and a new payload field to `hypershift-addon-operator`,
 increasing the surface area that must be kept in sync between `hcp` CLI releases and the
 addon operator's proxy version.
* Until the proxy-side work for W1/W2/W4 lands, `from-hub create` continues to either reject
 flags outright (`--version-check`) or silently accept but not fully deliver resources
 (`ExtraObjects` client-side done, server-side pending) — this enhancement does not change
 behavior by itself; it is the design record that unblocks the pending Jira work.

## Alternatives (Not Implemented)

**Point `util.GetConfig()`/`util.GetClient()` at a ManagedCluster/hosting-cluster kubeconfig
fetched from the hub**, and let existing `hcp create` logic run unchanged against the hosting
cluster.

Rejected because it undoes the product goal of `hcp from-hub` (creating/editing/deleting
`HostedCluster`s without direct hosting-cluster kubeconfig access), elevates privilege (a
ManagedCluster admin kubeconfig vs. the caller's own hub identity impersonated through the
proxy), and bypasses the proxy's `managedcluster:admin`/impersonation RBAC and audit story.
See the full comparison table and phased-delivery rationale already recorded in
[FROM-HUB-ISSUES.md § Why keep the proxy architecture](FROM-HUB-ISSUES.md#why-keep-the-proxy-architecture-not-hosting-cluster-kubeconfig).
A hosting kubeconfig is only acceptable as an explicit, documented advanced/dev opt-in (e.g.
`--hosting-kubeconfig`) — never as the silent default.

**Add one typed `CreateRequest` field per rendered object `Kind`** (e.g. `Roles`,
`ConfigMaps`) instead of the generic `ExtraObjects` passthrough (W4). Rejected because it only
fixes the kinds we remember to add today; a future platform change emitting a new kind would
silently reintroduce the same drop bug that Issues 4 and 5 describe.

## Open Questions [optional]

1. Should the proxy's `ExtraObjects` apply step be transactional with `HostedCluster`/`NodePool`
 creation (all-or-nothing), or is "apply `ExtraObjects` first, then fail the whole request if
 any `HostedCluster`/`NodePool`/`Secret` step fails afterward" sufficient? This affects whether
 partially-applied `ExtraObjects` need cleanup on failure.
2. Should the version/supported-versions GET (W1/W2) require a namespace path segment at all,
 given the source ConfigMap always lives in the hosting cluster's `hypershift` namespace? Kept
 in the current sketch only for `APIResourceList` consistency with other proxy routes.
3. Once W1's real hosting-cluster check is implemented, should `--version-check` go back to
 being accepted (and perform the real check) automatically, or should from-hub keep rejecting
 it until an explicit opt-in flag confirms the operator wants the (potentially slower,
 extra-round-trip) hosting-cluster check?

## Test Plan

* Unit tests in `product-cli/cmd/fromhub/create_test.go` for: `wantVersionCheck` capture and
 forced `coreOpts.VersionCheck = false`; release-stream-to-release-image resolution and
 `ReleaseStream` clearing; Agent `APIServerAddress` defaulting when unset vs. respecting an
 explicit flag value; `buildRequestFromFile` routing unrecognized `Kind`s into `ExtraObjects`
 (already covered for the client-side W4 change).
* `hypershift-addon-operator` unit/integration tests for the new version/supported-versions GET
 handler (ConfigMap present/absent/malformed) and for `handleCreate` applying `ExtraObjects`
 (success, partial-failure-fails-whole-request, unsupported `Kind`).
* End-to-end coverage: an ACM/HyperShift-addon e2e scenario that runs
 `hcp from-hub create agent --version-check --release-stream ...` against a hub + hosting
 cluster pair and asserts the created `HostedCluster` plus its `capi-provider-role` `Role`
 exist on the **hosting** cluster, and that the version/release checks used the hosting
 cluster's operator/supported-versions, not the hub's.
* This does not touch the NodePool config hash path, so no additional
 `e2e-aws-upgrade-hypershift-operator` requirement is introduced by this enhancement itself.

## Graduation Criteria

**Note:** *Section not required until targeted at a release.*

### Dev Preview -> Tech Preview

* W1/W2 proxy endpoints and W4 server-side `ExtraObjects` application implemented and covered
 by the test plan above, gated behind the existing `hcp from-hub` command surface (no new
 top-level feature gate needed).
* `docs/content/how-to/common/from-hub-cli.md` documents the hosting-cluster-correct behavior
 of `--version-check`/`--release-stream` and the `--base-domain`-required/Agent-address-default
 behavior (W3).

### Tech Preview -> GA

* `--version-check` accepted again (no longer rejected) once W1 lands, with hosting-cluster
 comparison verified in CI across at least Agent and AWS platforms.
* `ExtraObjects` server-side application exercised in upgrade/rollback testing to confirm no
 orphaned objects are left on the hosting cluster on partial failure.

## Upgrade / Downgrade Strategy

The HCP proxy (`hypershift-addon-operator`) and the `hcp` CLI can be versioned/upgraded
independently. An older CLI without W1/W2/W4 client-side changes continues to work against a
newer proxy (it simply doesn't call the new endpoints or populate `ExtraObjects`). A newer CLI
calling the new version/supported-versions endpoint or sending `ExtraObjects` against an older
proxy that doesn't implement them yet must fail closed with a clear "not supported by this
proxy version" error rather than silently proceeding as if the hosting-cluster check/resource
delivery succeeded — this should be based on a proxy version/capability check, not solely on
HTTP 404, so a network error doesn't get misinterpreted as "capability unsupported."

## Version Skew Strategy

During rollout, expect skew between `hcp` CLI versions and the `hypershift-addon-operator`
proxy version running on a given hub. The CLI must treat W1/W2/W4 endpoints as capabilities the
proxy may not yet have, and fail closed with an actionable error (not a silent no-op) when they
are missing, exactly as it does today for `--version-check` under the interim rejection
behavior.

## Operational Aspects of API Extensions

* SLI: HCP proxy route latency/error rate for `GET .../version` and the `ExtraObjects` portion
 of `POST .../create`, alongside existing proxy create-route metrics.
* Impact on existing SLIs: one additional GET per `from-hub create` invocation when
 `--version-check` or a `--release-stream` needs resolving; negligible additional latency
 (single ConfigMap read on the hosting spoke). `ExtraObjects` application adds a small,
 bounded number of additional object creates (typically 0–2) to each create request; no impact
 on unrelated proxy routes.
* Failure modes: if the version/supported-versions GET fails, `from-hub create` fails closed
 (Risks and Mitigations); if `ExtraObjects` application fails on the hosting cluster, the whole
 create request must fail rather than leave a `HostedCluster` referencing missing objects.
* Escalation: `hypershift-addon-operator` maintainers for proxy-side failures; HyperShift CLI
 maintainers for client-side `from-hub create` failures.

## Support Procedures

* Detect: `from-hub create` returns an explicit error naming the failed step (version check,
 release resolution, or `ExtraObjects` application) rather than a generic HTTP failure: proxy
 logs will show the corresponding route and hosting-cluster read/write error.
* Disable: an operator can always bypass W1/W2 by passing `--release-image` directly and
 omitting `--version-check`; there is no way to bypass W4 short of the platform not rendering
 extra objects, since it is required for correctness (a `HostedCluster` that depends on a
 dropped `Role`/`ConfigMap` will not function correctly regardless).
* Consequences of the proxy's `ExtraObjects` handler being unavailable/misbehaving: creates for
 platforms that render extra objects (e.g. Agent) will fail closed instead of silently
 producing a broken `HostedCluster`, once the fail-closed behavior in this enhancement is
 implemented.

## Infrastructure Needed [optional]

None beyond the `hypershift-addon-operator` proxy changes tracked in
[ACM-39227](https://redhat.atlassian.net/browse/ACM-39227),
[ACM-39228](https://redhat.atlassian.net/browse/ACM-39228), and
[ACM-39216](https://redhat.atlassian.net/browse/ACM-39216).
