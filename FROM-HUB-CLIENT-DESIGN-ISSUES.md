# `hcp from-hub` Issues Not Caused by Reusing Core-CLI Code

## Summary

These bugs live in `product-cli/cmd/fromhub/` — the same package covered by [FROM-HUB-ISSUES.md](FROM-HUB-ISSUES.md) — but unlike the issues in that document, none of them are a *collision from reusing* `product-cli`/core-CLI code (`core.CreateCluster`, `core.BindOptions`, shared platform `Complete()`/validation paths, etc.) that assumes hosting-cluster kubeconfig access. Every issue here is in from-hub's **own** code.

Two flavors show up:

- **Issues A–C:** independent client-side design/protocol bugs (mismatched HTTP wire contracts, duplicated flag/option bindings). These don't involve wrong-cluster targeting at all — the code already correctly reaches the **hosting** cluster via the proxy (`buildHTTPClient`/`hostedClusterURL`).
- **Issue D:** a genuine wrong-cluster-targeting bug (`util.GetClient()` hitting the hub), same *symptom* as the issues in `FROM-HUB-ISSUES.md` — but it's from-hub's own code independently making that mistake, not a collision with reused core-CLI logic, so it's grouped here instead.

They were split out of `FROM-HUB-ISSUES.md` to keep that document focused specifically on the reused-core-CLI-code root cause.

---

## Issue A. `edit` and `delete` decode GET responses differently (High / likely)

**Problem:**

- `delete.getHostedClusterFromProxy` expects `{ "hostedCluster": ... }`
- `edit.runEdit` decodes the body as a raw `HostedCluster`

If the proxy returns a wrapped object, edit operates on an empty/wrong object and produces bad patches.

**Expected:** One shared GET decoder matching the real proxy response contract.

**Code:** `product-cli/cmd/fromhub/delete.go`; `product-cli/cmd/fromhub/edit.go`

---

## Issue B. `edit` sends strategic merge patch as JSON merge patch (Medium)

**Problem:** `edit.go:114` creates a **strategic merge patch** via `strategicpatch.CreateTwoWayMergePatch`, but sends it with `Content-Type: application/merge-patch+json` (line 129). These are different patch semantics — strategic merge patch merges list items by Kubernetes merge keys, while JSON merge patch (RFC 7386) replaces arrays wholesale.

For scalar field edits this works fine, but editing list fields (e.g. `spec.services`, `spec.networking.serviceNetwork`) could silently produce incorrect results — the server interprets the patch as a full list replacement instead of a key-based merge.

**Expected:** Either use `application/strategic-merge-patch+json` as the content type, or generate a JSON merge patch instead of a strategic merge patch.

**Code:** `product-cli/cmd/fromhub/edit.go` → `runEdit` (lines 114, 129)

---

## Issue C. Namespace flag semantics are split across option structs (Low)

**Problem:**

- `from-hub` persistent `--namespace` binds to `fromhub.Options.Namespace` (used by delete/edit/proxy fallback)
- `from-hub create` also binds `--namespace` via `core.BindOptions` to `core.RawCreateOptions.Namespace` (used for rendered resources)

Create happens to prefer `HostedCluster.Namespace` when posting, but the dual binding is confusing and easy to break.

**Expected:** Single namespace source of truth for all from-hub subcommands.

**Code:** `product-cli/cmd/fromhub/fromhub.go`; `product-cli/cmd/fromhub/create.go`

---

## Issue D. ~~`from-hub delete aws` finalizer cleanup targets wrong cluster~~ (Medium) — Tracked

**Problem:** `removeHCFinalizer` uses `util.GetClient()` and patches the HostedCluster locally. The object lives on the hosting cluster and is only reachable via the HCP proxy. Failure is treated as non-fatal, so the HC can remain stuck terminating.

Unlike Issues A–C, this **is** a wrong-cluster-targeting bug — but `removeHCFinalizer` is from-hub's own code in `delete.go`, not a reused `product-cli`/core-CLI path, so it doesn't belong in `FROM-HUB-ISSUES.md`'s reused-code root cause.

**Expected:** Remove finalizers through the proxy (or a dedicated proxy endpoint), not via hub/default kubeconfig.

**Code:** `product-cli/cmd/fromhub/delete.go` → `removeHCFinalizer`

**Status:** No fix implemented yet in either repo. Requires a new server-side HCP Proxy endpoint before the client can be fixed — tracked in [ACM-39226](https://redhat.atlassian.net/browse/ACM-39226) (child of the HCP Proxy epic [ACM-37265](https://redhat.atlassian.net/browse/ACM-37265)): add a finalizer-removal endpoint to the proxy (`hypershift-addon-operator`), then rewrite `removeHCFinalizer` in this repo to call it via `buildHTTPClient`/`hostedClusterURL` instead of `util.GetClient()`, and propagate real errors instead of always returning `nil`.

---

## Suggested Fix Direction

1. Extract the GET decode logic in `delete.getHostedClusterFromProxy` into a shared helper (e.g. in `client.go`), and have `edit.runEdit` call it instead of decoding the raw body itself (Issue A).
2. In `edit.go`, either switch the `Content-Type` header to `application/strategic-merge-patch+json` to match the patch actually generated, or generate a JSON merge patch (e.g. via `jsonmergepatch` / a manual diff) if the proxy only supports RFC 7386 merge patches (Issue B). Confirm which patch type the proxy (`hypershift-addon-operator`) actually supports before choosing.
3. Pick a single namespace source of truth for all from-hub subcommands — e.g. stop binding `--namespace` a second time via `core.BindOptions` in `create.go`, and have `create` read `fromhub.Options.Namespace` directly (falling back to `HostedCluster.Namespace` from the rendered manifest only when the flag is unset) (Issue C).
4. Add a finalizer-removal endpoint to the HCP proxy (`hypershift-addon-operator`), then rewrite `removeHCFinalizer` to call it via `buildHTTPClient`/`hostedClusterURL` instead of `util.GetClient()`, and propagate real errors instead of always returning `nil` (Issue D — [ACM-39226](https://redhat.atlassian.net/browse/ACM-39226)).

## Acceptance Criteria

- [ ] `from-hub edit` and `from-hub delete` share one GET decoder that matches the real proxy response contract (Issue A)
- [ ] `from-hub edit` correctly loads and patches the live HostedCluster from the proxy (Issue A)
- [ ] `from-hub edit`'s PATCH `Content-Type` matches the patch type it actually generates (Issue B)
- [ ] `--namespace` has a single source of truth shared by all from-hub subcommands (create/edit/delete) (Issue C)
- [ ] `from-hub delete aws` finalizer cleanup affects the hosting-cluster object (or is removed if unsupported) (Issue D, proxy endpoint: [ACM-39226](https://redhat.atlassian.net/browse/ACM-39226))

## References

- CLI: `product-cli/cmd/fromhub/edit.go`, `product-cli/cmd/fromhub/delete.go`, `product-cli/cmd/fromhub/create.go`, `product-cli/cmd/fromhub/fromhub.go`
- Related: [FROM-HUB-ISSUES.md](FROM-HUB-ISSUES.md) (issues from reusing core-CLI code, the broader from-hub issue set these were split out of)
