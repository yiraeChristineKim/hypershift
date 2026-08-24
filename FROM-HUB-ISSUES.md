# `hcp from-hub` Issues: Hub vs Hosting Cluster Confusion

## Summary

`hcp from-hub` reuses product-cli create/destroy logic that assumes `$KUBECONFIG` points at the **hosting / management cluster**. In the from-hub flow, the CLI connects to the **hub** and applies resources to the hosting cluster only through the HCP proxy (`hcp.ocm.io/v1alpha1`).

As a result, several validations, lookups, and side effects run against the wrong cluster (hub), while required rendered resources are dropped before the proxy POST.

```
Intended path:
  hcp CLI → hub kube-apiserver → HCP proxy → hosting cluster

Broken assumption in reused product-cli code:
  util.GetClient() / util.GetConfig() → default $KUBECONFIG (hub)
```

---



## Root Cause

`hcp from-hub create` calls `core.CreateCluster` in render mode (`product-cli/cmd/fromhub/create.go`).

That path still uses:

- `util.GetClient()`
- `util.GetConfig()`

Those helpers **ignore** `--hub-kubeconfig` and `--hosting-cluster`. They talk to whatever the default kubeconfig/context is (almost always the hub).

Relevant entry points:


| Area                      | File                                | Problem                                                  |
| ------------------------- | ----------------------------------- | -------------------------------------------------------- |
| Create via render + proxy | `product-cli/cmd/fromhub/create.go` | Reuses `core.CreateCluster`; drops non HC/NP/Secret docs |
| Hub HTTP client           | `product-cli/cmd/fromhub/client.go` | Correctly uses `--hub-kubeconfig` for proxy only         |
| Shared create validation  | `cmd/cluster/core/create.go`        | Version check, release-stream, existence/arch checks     |
| Agent complete            | `cmd/cluster/agent/create.go`       | Resolves API server address from kubeconfig nodes        |


---



## Issues



### 1. `--version-check` compares against hub HyperShift Operator (High)

**Problem:** With `--version-check`, `validateVersion` reads the `supported-versions` ConfigMap via `util.GetClient()`. That is the **hub** operator version, not the hosting cluster operator version.

**Expected:** Compare CLI version against the HyperShift Operator on the **hosting** cluster (via proxy or an equivalent hosting-cluster API).

**Code:** `cmd/cluster/core/create.go` → `validateVersionAndWait` / `validateVersion`

**Workaround (from-hub create):** Disable core's `--version-check` before calling `CreateCluster` (`coreOpts.VersionCheck = false`) so it never checks the hub. If the user passed `--version-check`, perform the comparison against the **hosting** cluster via the HCP proxy instead. See [Workarounds](#workarounds) W1.

**Interim status:** As of the Issue 9 fix, `--version-check` is now explicitly rejected for `hcp from-hub create` (clear error if set) instead of silently checking the hub — the "actively wrong" behavior described above no longer happens. The full W1 workaround (comparing against the **hosting** cluster via the proxy) is still not implemented, so `--version-check` currently has no working equivalent for from-hub; it just fails closed. The blocking piece — a proxy endpoint to read the hosting cluster's HyperShift Operator version — is tracked in [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227) (child of the HCP Proxy epic [ACM-37265](https://redhat.atlassian.net/browse/ACM-37265)). Once that lands, `from-hub create` can be updated to always force `VersionCheck=false` internally and, only when the user passes `--version-check`, ping the proxy for the hosting HO version instead of rejecting the flag outright.

---



### 2. `--release-stream` defaults using hub supported versions (High)

**Problem:** When `--release-image` is unset and `--release-stream` is set, `resolveReleaseImage` → `LookupDefaultOCPVersion` lists ConfigMaps labeled `hypershift.openshift.io/supported-versions` through `util.GetClient()`.

**Expected:** Default/filter release versions using the **hosting** cluster HO supported versions.

**Code:** `cmd/cluster/core/create.go` → `resolveReleaseImage`; `support/supportedversion/version.go` → `LookupDefaultOCPVersion` / `retrieveSupportedOCPVersion`

**Workaround (from-hub create):** Resolve the release image in from-hub via the HCP proxy (hosting supported-versions), set `coreOpts.ReleaseImage`, and clear `ReleaseStream` before `CreateCluster` so core never calls `util.GetClient()`. See [Workarounds](#workarounds) W2.

**Status:** No fix implemented yet. Requires a new (or extended) server-side HCP Proxy endpoint before the client can be fixed — tracked in [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228) (child of the HCP Proxy epic [ACM-37265](https://redhat.atlassian.net/browse/ACM-37265)), which shares the same endpoint family as `--version-check`'s [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227).

---



### 3. Agent: API server address taken from hub nodes (High)

**Problem:** If `--api-server-address` is unset, agent `Complete()` calls `GetAPIServerAddressByNode()`, which reads a node IP from the default `$KUBECONFIG`/current context (not `--hub-kubeconfig`, and not the hosting cluster — in practice this is usually the hub, but only incidentally) and embeds it into HostedCluster service publishing.

**Expected:** Resolve from the **hosting** cluster, or require `--api-server-address` explicitly for from-hub.

**Note:** KubeVirt has the same `GetAPIServerAddressByNode` call for NodePort strategy (`cmd/cluster/kubevirt/create.go:83`), but it is guarded by `!opts.Render` so it is skipped in the from-hub render flow. The Agent platform's `Complete()` has **no** render-mode guard, which is why only Agent is broken.

**Code:** `cmd/cluster/agent/create.go` → `Complete`; `cmd/cluster/core/create.go` → `GetAPIServerAddressByNode`

**Workaround (from-hub create agent):** If `--api-server-address` is unset, from-hub defaults it to `api.<name>.<base-domain>` (same DNS-name convention as `hcp create cluster agent`) instead of calling `GetAPIServerAddressByNode` against the hub. This is why `--base-domain` is now a **required** field on `hcp from-hub create` (see W3) — unlike upstream `hcp create cluster agent`, where it is optional. See [Workarounds](#workarounds) W3.

---



### 4. Agent: `Role` rendered then dropped before reaching the hosting cluster (High)

**Problem:** Agent `GenerateResources()` emits `capi-provider-role` (`Role`). Previously, `buildRequestFromFile` only kept `HostedCluster`, `NodePool`, and `Secret`, so the Role was dropped client-side before the proxy POST. That part is now fixed (see Workarounds W4 — client-side landed): unmatched `Kind`s are captured into `CreateRequest.ExtraObjects` and sent to the proxy. The Role still never reaches the hosting cluster today, though, because the HCP proxy (`hypershift-addon-operator`) doesn't decode/apply `ExtraObjects` yet — the drop has moved server-side.

**Expected:** Include Role (and any other required non-secret objects) in the proxy create payload, and have the proxy create them on the hosting cluster.

**Code:** `cmd/cluster/agent/create.go` → `GenerateResources`; `product-cli/cmd/fromhub/create.go` ~~→~~ `buildRequestFromFile` ~~(client-side, done)~~; `hypershift-addon-operator` `pkg/manager/hcp_proxy.go` → `handleCreate` (server-side, pending — [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216))

**Workaround (from-hub create):** Client-side: a generic passthrough field (`ExtraObjects`) on `CreateRequest` captures any rendered object whose `Kind` isn't `HostedCluster`/`NodePool`/`Secret`, so `Role` (and anything else a platform's `GenerateResources()` emits) is no longer silently dropped before the proxy POST. See [Workarounds](#workarounds) W4. Server-side application of `ExtraObjects` on the hosting cluster is tracked separately in [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216).

---



### 5. ConfigMaps dropped from create payload (High)

**Problem:** Same parser gap drops ConfigMaps such as:

- `--additional-trust-bundle` → `user-ca-bundle`
- AWS proxy CA ConfigMap (when proxy is enabled)

The HostedCluster may reference objects that were never created on the hosting cluster. As with Issue 4, the client-side drop is now fixed (ConfigMaps land in `ExtraObjects` too), but they still don't reach the hosting cluster until the proxy handles `ExtraObjects` server-side.

**Expected:** Forward ConfigMaps (and other required objects) through the proxy create request, and have the proxy create them on the hosting cluster.

**Code:** `product-cli/cmd/fromhub/create.go` ~~→~~ `buildRequestFromFile`~~;~~ `product-cli/cmd/fromhub/types.go` ~~→~~ `CreateRequest` ~~(client-side, done)~~; `hypershift-addon-operator` `pkg/manager/hcp_proxy.go` → `handleCreate` (server-side, pending — [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216))

**Workaround (from-hub create):** Same generic passthrough mechanism as Issue 4 — ConfigMaps have no dedicated `CreateRequest` field either, so they fall into the same `ExtraObjects` bucket and are fixed client-side by the same change. See [Workarounds](#workarounds) W4. Server-side work tracked in [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216).

---



### 6. `--secret-creds` ~~looks up secrets on the wrong cluster~~ (Medium) — Fixed

**Problem:** AWS create reads `--secret-creds` via `util.GetClient()` / `util.GetSecret` against hub/default kubeconfig. For from-hub, that secret is either missing or belongs to the wrong cluster.

**Expected:** Disallow `--secret-creds` for from-hub, or document/require local file-based credentials only; do not read hosting secrets from the hub.

**Code:** `cmd/cluster/aws/create.go` → `Complete` / `ExtractOptionsFromSecret`

**Fix (from-hub create aws):** `product-cli/cmd/fromhub/create.go` adds `awsUnsupportedFlags` (same mechanism as Issue 9's `unsupportedFlags`, generalized into shared `hideFlags`/`rejectFlags` helpers) containing `--secret-creds`. It is hidden from `hcp from-hub create aws --help`, and `newCreateAWSCommand`'s `RunE` rejects it with a clear error if set explicitly — for this release, from-hub only supports local file-based AWS credentials (`--pull-secret`, `--sts-creds`, `--role-arn`, …), never a Secret read from the default (hub) kubeconfig context. Unit tests: `product-cli/cmd/fromhub/create_test.go` (`TestAWSUnsupportedFlags`).

---

*(Issue 7 —* `delete aws` *finalizer cleanup targets wrong cluster — moved to [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md) as Issue D; it's a real wrong-cluster bug, but from-hub's own code, not a collision with reused core-CLI logic.)*

---

*(Issue 8 —* `edit`*/*`delete` *GET-decode and patch-content-type bugs — moved to [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md); not a hub-vs-hosting-cluster issue.)*

---



### 9. ~~Misleading product-cli flags inherited by from-hub~~ (Low) — Fixed

Because `core.BindOptions` is reused, from-hub create still exposes flags that are wrong or no-ops in this flow:


| Flag                                                | Why it is wrong for from-hub                                                                                                                                                                                                                                                      |
| --------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--wait` / `--timeout`                              | No-op; create always uses render + proxy                                                                                                                                                                                                                                          |
| `--render` / `--render-into` / `--render-sensitive` | from-hub has no direct-apply alternative to render in the first place — it always renders internally (to a temp file) purely as an implementation detail to build the payload it POSTs to the proxy, so there is no user-facing render-vs-apply choice for these flags to control |


`--version-check` has the same kind of problem but is covered in detail in Issue 1, since it needs its own explanation (interim rejection now, real hosting-cluster check still blocked on [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227)) rather than a one-line "wrong cluster" note.

**Expected:** Bind a from-hub-specific flag set, or hide/disable flags that do not apply.

**Fix (from-hub create):** `product-cli/cmd/fromhub/create.go` now declares `unsupportedFlags` for six flags — these plus `--version-check` (Issue 1). `hideUnsupportedFlags` marks them hidden on `cmd.PersistentFlags()` in `newCreateCommand`, so they no longer appear in `hcp from-hub create --help` or any platform subcommand's help. `rejectUnsupportedFlags` runs at the top of `runViaProxy` and returns a clear, flag-specific error if a caller sets any of them explicitly (via `cmd.Flags().Changed(...)`), instead of silently no-op'ing (`--wait`/`--timeout`/`--render`*). Unit tests: `product-cli/cmd/fromhub/create_test.go`.

---



### 10. Hosting-cluster validations skipped in render mode (Medium)

**Problem:** `validateClusterExistence` returns early when `Render=true`, so from-hub skips:

- "HostedCluster already exists" check on the target cluster
- management vs NodePool CPU architecture check

**Expected:** Perform these checks against the **hosting** cluster via proxy (or equivalent), instead of skipping them.

**Code:** `cmd/cluster/core/create.go` → `validateClusterExistence`

---

*(Issue 11 — namespace flag semantics split across option structs — moved to [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md); not a hub-vs-hosting-cluster issue.)*

---



## Workarounds

Concrete from-hub CLI changes that keep the proxy architecture and avoid `util.GetClient()` pointing at the hub for management-cluster semantics.

### W1. `--version-check` via HCP proxy (Issue 1)

**Status:** Interim mitigation done — `--version-check` is rejected outright for from-hub (Issue 9's `unsupportedFlags`), so it no longer silently checks the hub. The real workaround below (checking against the **hosting** cluster) is blocked on a proxy endpoint, tracked in [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227).

**Goal:** If the user passes `--version-check`, compare the CLI commit SHA against the **hosting** cluster HyperShift Operator version — never against the hub.

**from-hub create flow (**`runViaProxy`**):**

```text
1. Capture user intent:
     wantVersionCheck := coreOpts.VersionCheck

2. Always disable core's check before calling CreateCluster:
     coreOpts.VersionCheck = false
   so validateVersion() / util.GetClient() never runs against the hub.

3. If wantVersionCheck:
     a. GET hosting operator version from the HCP proxy
        (hostingCluster query param = --hosting-cluster)
     b. Compare to supportedversion.GetRevision() (CLI SHA)
     c. On mismatch → return error (same message shape as core):
        "version mismatch detected, CLI: %s, Operator: %s"
     d. On proxy/version unavailable → fail closed (do not silently skip)

4. Then render via core.CreateCluster (VersionCheck still false)
   and POST CreateRequest through the proxy as today.
```

**Sketch (client):**

```go
func runViaProxy(...) error {
    wantVersionCheck := coreOpts.VersionCheck
    coreOpts.VersionCheck = false // never let core check hub HO

    if wantVersionCheck {
        if err := validateHostingVersion(cmd.Context(), hubOpts); err != nil {
            return err
        }
    }

    // existing render + postToProxy ...
}

func validateHostingVersion(ctx context.Context, opts *Options) error {
    operatorVersion, err := getHostingOperatorVersion(ctx, opts)
    if err != nil {
        return fmt.Errorf("failed to get hosting operator version from proxy: %w", err)
    }
    cliVersion := supportedversion.GetRevision()
    if operatorVersion != cliVersion {
        return fmt.Errorf("version mismatch detected, CLI: %s, Operator: %s", cliVersion, operatorVersion)
    }
    return nil
}
```

**Proxy requirement:** The HCP proxy must expose a way to read the hosting cluster HO version (the `server-version` key from the hosting `supported-versions` ConfigMap in the `hypershift` namespace — same source `validateVersion` uses today).

```text
GET /apis/hcp.ocm.io/v1alpha1/namespaces/{ns}/version?hostingCluster={name}
```

```json
{
  "serverVersion": "<hosting-ho-git-sha>"
}
```

Auth/RBAC for this GET should match other proxy routes (`managedcluster:admin` + impersonation). Namespace can be ignored for version lookup if the ConfigMap always lives in `hypershift` on the hosting cluster; keep `namespaces/{ns}` only if needed for APIResourceList consistency.

**Files to touch:**


| Side  | File                                                      | Change                                                                                                                                                                                                           |
| ----- | --------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CLI   | `product-cli/cmd/fromhub/create.go`                       | Remove `version-check` from `unsupportedFlags`; capture `VersionCheck`, force `false` before `CreateCluster`, call proxy check if requested — pending [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227) |
| CLI   | `product-cli/cmd/fromhub/client.go` (or new `version.go`) | HTTP GET + decode for hosting `serverVersion` — pending [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227)                                                                                               |
| Proxy | `hypershift-addon-operator` `pkg/manager/hcp_proxy.go`    | Route + spoke GET of `supported-versions` ConfigMap → return `serverVersion` — pending [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227)                                                                |


**Relationship to W2:** Both need one hosting-side metadata GET; implement as a single endpoint returning both `serverVersion` and `supportedVersions` to avoid two round trips.

---



### W2. `--release-stream` defaults via HCP proxy (Issue 2)

**Status:** No fix implemented yet. Blocked on a server-side HCP Proxy endpoint returning the hosting cluster's `supportedVersions`, tracked in [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228) (same endpoint family as W1's [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227)).

**Goal:** Default/filter OpenShift release versions using the **hosting** cluster's HyperShift Operator supported-versions — never the hub's.

**What** `--release-stream` **is:** Selects an **OpenShift (OCP) release version** from a named stream (e.g. `4-stable-multi`) instead of pinning a full image with `--release-image`. The CLI looks up tags for that stream, filters them against the HyperShift Operator's `supported-versions` ConfigMap, and sets a concrete release image pull spec. This is **not** the HyperShift operator / CLI commit version (that is `--version-check` / W1).

**from-hub create flow (**`runViaProxy`**), before** `CreateCluster`**:**

```text
1. If coreOpts.ReleaseImage is already set: do nothing, proceed as today.

2. Else if coreOpts.ReleaseStream is set:
     a. GET hosting supported-versions from the HCP proxy
        (same endpoint/response as W1, field: supportedVersions)
     b. Resolve the release-stream tags (same lookup core does today)
        filtered against the hosting supportedVersions list
     c. Set coreOpts.ReleaseImage = <resolved pull spec>
     d. Clear coreOpts.ReleaseStream so core's own
        resolveReleaseImage() / LookupDefaultOCPVersion() never run
        (they would otherwise call util.GetClient() against the hub)

3. Then core.CreateCluster (render) + POST via proxy as today.
```

Client-side resolution can call into existing `supportedversion` helpers if they accept an in-memory ConfigMap / version list; otherwise duplicate the "latest non-RC tag on the stream that is in supportedVersions" selection in from-hub, using the public release-stream HTTP APIs the same way `retrieveSupportedOCPVersion` does today.

**Files to touch:**


| Side  | File                                                   | Change                                                                                                                                              |
| ----- | ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| CLI   | `product-cli/cmd/fromhub/create.go`                    | Resolve stream → image via proxy; clear `ReleaseStream` before `CreateCluster` — pending [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228) |
| CLI   | `product-cli/cmd/fromhub/client.go` (or `version.go`)  | Shared GET for hosting `serverVersion` + `supportedVersions` — pending [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228)                   |
| CLI   | optional helper next to `supportedversion`             | Resolve pull spec from stream + in-memory supported versions (no kube client) — pending [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228)  |
| Proxy | `hypershift-addon-operator` `pkg/manager/hcp_proxy.go` | Same metadata route as W1; include `supportedVersions` from spoke ConfigMap — pending [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228)    |


**Relationship to W1:** One proxy GET should serve both workarounds. W1 uses `serverVersion`; W2 uses `supportedVersions`. (W3 needs no proxy call — see below.)

---



### W3. Agent API server address: default to a DNS name, not a node IP (Issue 3)

**What goes wrong today:** Agent clusters need an API server address in the HostedCluster spec (how agents reach the API). If `--api-server-address` is unset, `agent.Complete()` calls `GetAPIServerAddressByNode()` — it picks **one node's IP** from whatever `$KUBECONFIG` points at. For from-hub, that is usually the **hub**, not the hosting cluster, so the wrong IP can be baked into the spec. Even a hosting node IP is fragile: if that node is drained or replaced, the address in the spec goes stale.

**What W3 does instead:** Before `CreateCluster`, from-hub sets `agentOpts.APIServerAddress = "api.<name>.<base-domain>"` (same convention as `hcp create cluster agent`). That is a **DNS name** the operator points at hosting-cluster node IPs — not a hardcoded IP from any cluster lookup. `Complete()` sees the address already set and skips `GetAPIServerAddressByNode()` entirely. No proxy call and no kube API lookup needed.

**Example:** `--name myhc`, `--base-domain example.com` → default address `api.myhc.example.com`. The operator creates DNS `A` records for that name toward the hosting cluster's nodes; from-hub does **not** create or validate DNS.

**from-hub create agent flow (`runViaProxy`), before `CreateCluster`:**

```text
1. If user already passed --api-server-address:
     leave agentOpts.APIServerAddress as-is. Done.

2. Else:
     agentOpts.APIServerAddress = "api.<name>.<base-domain>"
     (from coreOpts.Name and coreOpts.BaseDomain).
     No proxy call, no node lookup — derived from flags only.

3. Then core.CreateCluster (render) + POST via proxy as today.
```

**Why `--base-domain` is required on from-hub:** Upstream `hcp create cluster agent` can fall back to `"example.com"` when base domain is empty; from-hub cannot — `api.<name>.example.com` will not resolve in production. from-hub therefore marks `--base-domain` required on all `hcp from-hub create` platforms (`product-cli/cmd/fromhub/create.go`), not only Agent. The same wrong-cluster reasoning applies to other platforms (e.g. KubeVirt `baseDomainPassthrough` would read the hub's apps domain, not the hosting cluster's).

**Sketch (client):**

```go
func runViaProxyAgent(...) error {
    // W1 / W2 as needed ...

    if agentOpts.APIServerAddress == "" {
        // Default to the same DNS-name convention `hcp create cluster agent` uses.
        // Requires the operator to ensure this name resolves to the hosting
        // cluster's node(s); from-hub does not create or validate the DNS record.
        agentOpts.APIServerAddress = fmt.Sprintf("api.%s.%s", coreOpts.Name, coreOpts.BaseDomain)
    }

    // CreateCluster → agent.Complete() sees APIServerAddress set → no hub node lookup
    return runViaProxy(cmd, hubOpts, coreOpts, agentOpts)
}
```

**Files to touch:**


| Side | File                                         | Change                                                                                                                                                                 |
| ---- | -------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CLI  | `product-cli/cmd/fromhub/create.go`          | Mark `--base-domain` required on `hcp from-hub create` (done); if agent and `--api-server-address` empty, default to `api.<name>.<base-domain>` before `CreateCluster` |
| Docs | `docs/content/how-to/common/from-hub-cli.md` | Document that `--base-domain` is required for from-hub (unlike product-cli), the DNS-name default for Agent, and that from-hub does not create DNS records             |


**Relationship to W1/W2:** None — W3 needs no proxy call, unlike W1 (`serverVersion`) and W2 (`supportedVersions`).

---



### W4. Generic passthrough for non-secret objects (Issues 4 and 5)

**Status:** Client-side (CLI) done. Server-side (proxy) pending — tracked in [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216).

**Goal:** Any object a platform's `GenerateResources()` renders — `Role`, `ConfigMap`, or anything else — reaches the hosting cluster, not just `HostedCluster`/`NodePool`/`Secret`.

**Why generic instead of one field per kind:** Today's problem isn't specific to Role or ConfigMap — `buildRequestFromFile`'s switch has no `default` case, so *any* unrecognized `Kind` is silently dropped. Adding a typed field per kind (`Roles`, `ConfigMaps`, …) only fixes the kinds we remember to add; a future platform change (e.g. a new ConfigMap or a RoleBinding) would silently reintroduce this bug. A single generic bucket for "everything else" fixes Issues 4 and 5 at once and is resilient to new kinds.

**from-hub create flow (**`buildRequestFromFile`**):**

```text
1. For each YAML doc in the rendered file, read Kind as today.
2. Route to the existing typed fields for HostedCluster / NodePool / Secret.
3. For any other non-empty Kind (Role, ConfigMap, RoleBinding, …):
     append the raw object to req.ExtraObjects instead of dropping it.
4. POST CreateRequest (now including ExtraObjects) via proxy as today.
```

**Sketch (client):**

```go
// types.go
type CreateRequest struct {
    HostedCluster *hypershiftv1beta1.HostedCluster `json:"hostedCluster"`
    NodePools     []*hypershiftv1beta1.NodePool     `json:"nodePools,omitempty"`
    Secrets       []corev1.Secret                   `json:"secrets,omitempty"`

    // ExtraObjects holds any other non-secret object emitted by a platform's
    // GenerateResources() (e.g. Agent's capi-provider-role Role, the
    // user-ca-bundle ConfigMap from --additional-trust-bundle) that doesn't
    // have a dedicated field above. The server creates each one verbatim on
    // the hosting cluster, in the given order, before the HostedCluster.
    ExtraObjects []runtime.RawExtension `json:"extraObjects,omitempty"`
}
```

```go
// create.go, inside buildRequestFromFile's per-doc loop
switch typeMeta.Kind {
case "HostedCluster":
    // ... unchanged ...
case "NodePool":
    // ... unchanged ...
case "Secret":
    // ... unchanged ...
default:
    req.ExtraObjects = append(req.ExtraObjects, runtime.RawExtension{Raw: doc})
}
```

`stampFromHubLabels` also needs to add `hcp.ocm.io/created-by` / `hcp.ocm.io/hostedcluster` to each `ExtraObjects` entry (decode metadata, set labels, re-encode), the same way it already does for `HostedCluster`/`NodePools`/`Secrets`.

**Proxy requirement:** Decode each `ExtraObjects` entry (Role, RBAC, and core API types are enough for known platforms) and create/apply it on the hosting spoke — in the same phase as Secrets, before the HostedCluster is created, since the HostedCluster/operator may assume these already exist.

**Files to touch:**


| Side  | File                                                   | Change                                                                                                                                                                    |
| ----- | ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CLI   | `product-cli/cmd/fromhub/types.go`                     | ~~Add~~ `ExtraObjects []runtime.RawExtension` ~~to~~ `CreateRequest` — **done**                                                                                           |
| CLI   | `product-cli/cmd/fromhub/create.go`                    | `buildRequestFromFile`~~:~~ `default` ~~case appends unmatched docs to~~ `ExtraObjects`~~;~~ `stampFromHubLabels`~~: label each~~ `ExtraObjects` ~~entry too~~ — **done** |
| Proxy | `hypershift-addon-operator` `pkg/manager/hcp_proxy.go` | Decode + create/apply each `ExtraObjects` entry on the hosting cluster before the HostedCluster — **pending, [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216)** |


**Relationship to Issues 4/5:** One mechanism fixes both — Issue 4's `Role` and Issue 5's `ConfigMap`s both land in `ExtraObjects`; no per-kind workaround needed.

---



## Suggested Fix Direction

**Chosen approach: keep the HCP proxy architecture; do not switch from-hub to a hosting-cluster / ManagedCluster kubeconfig.**

1. **Never treat hub kubeconfig as the management/hosting cluster** in from-hub create/delete paths.
2. Run version / supported-OCP checks against the **hosting** cluster (proxy API), or fail closed if unavailable.
3. Expand create payload beyond HC / NodePool / Secret with a generic passthrough for other rendered objects (at least ConfigMap + Role) (W4).
4. For agent from-hub create, default API server address to `api.<name>.<base-domain>` and set it on agent options before core (W3); never from hub nodes.
5. ~~Stop exposing product-cli-only flags (~~`--wait`~~,~~ `--render*`~~, hub-oriented~~ `--version-check`~~) on from-hub~~, or give them from-hub-correct behavior — **done** (Issue 9): hidden from `--help` and rejected if set explicitly.
6. Do **not** override `util.GetConfig()` / `util.GetClient()` with a ManagedCluster kubeconfig, and do **not** teach them `--hub-kubeconfig`. Stop from-hub from depending on those call sites for "management cluster" semantics; use proxy-backed checks (W1/W2) or flag-derived defaults (W3).

See [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md) for the `edit`/`delete` GET-decode/patch-content-type bugs, the `--namespace` option-struct duplication, and the `delete aws` finalizer wrong-cluster bug — none of which are collisions from reusing core-CLI code.

---



## Why keep the proxy architecture (not hosting-cluster kubeconfig)

A natural-looking fix is: obtain the hosting ManagedCluster kubeconfig from the hub, point `util.GetConfig()` at it, and let existing `hcp create` logic run unchanged against the hosting cluster.

We should **not** do that as the primary from-hub design. Keep:

```
hcp CLI → hub kubeconfig / user identity → HCP proxy → hosting cluster
```



### Product goal

`hcp from-hub` exists so users can create/edit/delete HostedClusters **from the hub without direct hosting-cluster kubeconfig access**.

Requiring a ManagedCluster admin kubeconfig (hub secret, downloaded kubeconfig, or cluster-proxy admin credentials) undoes that goal: many users who are allowed `managedcluster:admin` via the proxy path cannot read the ManagedCluster kubeconfig Secret.

### Security and identity model


| Proxy path (keep)                                                      | Hosting kubeconfig path (reject as default)                           |
| ---------------------------------------------------------------------- | --------------------------------------------------------------------- |
| Uses the caller's **hub** credentials                                  | Uses a **cluster-admin-style** hosting kubeconfig                     |
| Hub apiserver injects `X-Remote-User` / `X-Remote-Group`               | Identity is the kubeconfig's service account / cert, not the hub user |
| Proxy enforces `managedcluster:admin` then impersonates toward hosting | Bypasses proxy RBAC / clusterview checks                              |
| Hosting cluster enforces RBAC for the impersonated user                | Often over-privileged relative to the real operator                   |


Switching to ManagedCluster kubeconfig elevates privilege and breaks the audit/impersonation story the proxy was built for.

### Independence of the hcp CLI

Overriding `util.GetConfig()` globally to "fix" from-hub looks simple but is brittle:

- package-level / process-global config swaps are racy and hard to test
- temp `KUBECONFIG` files leak into unrelated code in the same process
- create would still need special cases for apply-via-proxy vs apply-direct

The durable way to keep product-cli reusable is a small injectable "management cluster view" (version, supported OCP versions, HC exists), with:

- `hcp create cluster` → `util.GetClient()` against the real management/hosting cluster
- `hcp from-hub` → proxy HTTP for those same facts, then local render + proxy POST

That preserves hcp CLI independence **without** pretending the proxy is a full kube-apiserver behind `rest.Config`.

### What hosting kubeconfig would still not fix

Even with a correct hosting `GetClient()`, these remain separate bugs:

- Agent `Role` / trust-bundle ConfigMaps dropped by `buildRequestFromFile`
- `edit`/`delete` GET-decode and patch-content-type mismatches, `--namespace` option-struct duplication, and the `delete aws` finalizer wrong-cluster bug (see [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md))
- misleading inherited flags (`--wait`, `--render*`)

So ManagedCluster kubeconfig is not a complete fix; it only addresses the "wrong cluster" class of `GetClient()` calls.

### How the proxy approach deals with `util.GetConfig()` instead

Do **not** feed the proxy into `util.GetConfig()`. The proxy is not a general kube API for ConfigMaps, Nodes, and Secrets.


| Today (broken)                                      | Proxy approach                                                                                              |
| --------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| from-hub create calls `util.GetClient()` → hub      | from-hub does not use `GetClient()` for management-cluster semantics                                        |
| `--version-check` / `--release-stream` read hub HO  | proxy returns hosting HO version / supported-versions                                                       |
| Agent resolves API address from hub nodes           | from-hub defaults to `api.<name>.<base-domain>`, sets agent opts before core (W3)                           |
| `--secret-creds` ~~reads Secret via~~ `GetClient()` | ~~disallow for from-hub; use local files (~~`--pull-secret`~~,~~ `--sts-creds`~~, …)~~ — **done** (Issue 6) |
| apply still intended via proxy POST                 | keep render locally + POST `CreateRequest` via proxy                                                        |


Phased delivery:

1. **Phase 0:** ~~disable/reject unsafe flags in from-hub~~ (`--version-check` / `--wait` / `--timeout` / `--render`* now rejected, Issue 9 — **done**; `--secret-creds` now rejected, Issue 6 — **done**; require `--release-image` still open); default agent `--api-server-address` to `api.<name>.<base-domain>` (W3, no proxy needed — `--base-domain` required flag done, the actual `APIServerAddress` defaulting logic is still open); fix dropped resources (client-side done, W4 — proxy side pending, [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216)).
2. **Phase 1:** proxy metadata endpoints + `ManagementClusterView` implemented by proxy for from-hub and by `util.GetClient()` for normal `hcp create`.
3. **Phase 2:** optional proxy helpers (finalizer patch, other metadata).



### When a hosting kubeconfig might still be acceptable

Only as an **explicit advanced/dev mode** (e.g. `--hosting-kubeconfig`), documented as bypassing the normal from-hub trust model — never as the silent default, and never by auto-loading the ManagedCluster admin Secret from the hub.

---



## Acceptance Criteria

- [ ] `--version-check` validates against hosting-cluster HO, not hub HO (W1, proxy endpoint: [ACM-39227](https://redhat.atlassian.net/browse/ACM-39227))
- [ ] `--release-stream` defaulting uses hosting-cluster supported versions (W2, proxy endpoint: [ACM-39228](https://redhat.atlassian.net/browse/ACM-39228))
- [ ] from-hub create does not call `util.GetClient()` for hosting validation/defaulting (`--hub-kubeconfig` stays proxy-only; not plumbed into core)
- [x] `hcp from-hub create` requires `--base-domain` explicitly (no hub-derived defaulting for Agent/KubeVirt) (`product-cli/cmd/fromhub/create.go`)
- [x] Misleading/no-op product-cli flags (`--version-check`, `--wait`, `--timeout`, `--render*`, AWS `--secret-creds`) are hidden and explicitly rejected on `hcp from-hub create`, not silently ignored or misapplied (Issues 6, 9; `product-cli/cmd/fromhub/create.go`, `create_test.go`)
- [ ] Agent from-hub create defaults API server address to `api.<name>.<base-domain>`, not hub nodes (W3)
- [ ] Agent Role and trust-bundle/proxy ConfigMaps are created on the hosting cluster (W4, server-side proxy work: [ACM-39216](https://redhat.atlassian.net/browse/ACM-39216))
- [ ] Docs updated for any intentional flag differences vs `hcp create cluster`

See [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md) for the `edit`/`delete` GET-decode/patch-content-type, `--namespace`, and `delete aws` finalizer acceptance criteria.

---



## References

- Docs: `docs/content/how-to/common/from-hub-cli.md`
- CLI: `product-cli/cmd/fromhub/`
- Shared create: `cmd/cluster/core/create.go`
- Client helpers: `cmd/util/client.go`
- Related (non-hub-vs-hosting bugs in the same package): [FROM-HUB-CLIENT-DESIGN-ISSUES.md](FROM-HUB-CLIENT-DESIGN-ISSUES.md)

