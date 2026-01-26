# Hext-Dev Fork Notes

This document tracks custom modifications in the hext-dev/envbuilder fork.

## Branch Strategy

| Branch | Purpose | Image Tag |
|--------|---------|-----------|
| `main` | Stable, synced with upstream | `ghcr.io/hext-dev/envbuilder:latest` |
| `hext/gcp-lifecycle-reporting` | GCP instance identity auth + lifecycle reporting | `ghcr.io/hext-dev/envbuilder:hext-dev-*` |

## Tag Namespace

- **Upstream tags:** `v1.x.x` (from coder/envbuilder)
- **Hext dev tags:** `hext-dev-v0.x.x` (our custom features)

This separation allows easy rollback:
```terraform
# Use stable version
devcontainer_builder_image = "ghcr.io/hext-dev/envbuilder:latest"

# Use dev version with GCP lifecycle reporting
devcontainer_builder_image = "ghcr.io/hext-dev/envbuilder:hext-dev-v0.1.0"
```

## Feature: GCP Instance Identity Auth + Lifecycle Reporting

**Branch:** `hext/gcp-lifecycle-reporting`

**Problem:** When envbuilder fails to build a devcontainer, the Coder workspace hangs forever because:
1. The Coder agent runs inside the container (never starts if build fails)
2. We can't use `CODER_AGENT_TOKEN` due to caching/agent association issues
3. No way to report `start_error` lifecycle state to Coder

**Solution:** Add GCP instance identity authentication to envbuilder, allowing it to:
1. Authenticate to Coder without a pre-existing agent token
2. Report `start_error` lifecycle state on build failure
3. Forward build logs to Coder UI

### New Environment Variables

| Variable | Description |
|----------|-------------|
| `ENVBUILDER_CODER_AUTH_METHOD` | Auth method: `token` (default) or `gcp-instance-identity` |
| `ENVBUILDER_GCP_SERVICE_ACCOUNT` | GCP service account email (required for gcp-instance-identity) |

### Implementation Steps

1. ✅ Add new options to `options/options.go`
2. ✅ Add GCP instance identity auth to `log/coder.go`
3. ✅ Add lifecycle reporting on failure in `cmd/envbuilder/main.go`
4. ⚠️ Update GitHub Actions to build `hext-dev-*` tags (requires manual edit via GitHub UI)

### Manual Step Required: Update GitHub Actions

The OAuth token lacks `workflow` scope, so you need to manually edit `.github/workflows/release.yaml` via the GitHub web UI:

1. Go to: https://github.com/hext-dev/envbuilder/edit/hext/gcp-lifecycle-reporting/.github/workflows/release.yaml
2. Change the `on.push.tags` section from:
   ```yaml
   on:
     push:
       tags:
         - "v*"
   ```
   To:
   ```yaml
   on:
     push:
       tags:
         - "v*"
         - "hext-dev-v*"
   ```
3. Commit directly to the `hext/gcp-lifecycle-reporting` branch

### Testing

```bash
# Template uses new image
devcontainer_builder_image = "ghcr.io/hext-dev/envbuilder:hext-dev-v0.1.0"

# Pass new env vars
"ENVBUILDER_CODER_AUTH_METHOD": "gcp-instance-identity",
"ENVBUILDER_GCP_SERVICE_ACCOUNT": var.service_account_email,
"CODER_AGENT_URL": data.coder_workspace.me.access_url,
```

## Syncing with Upstream

```bash
git fetch upstream
git checkout main
git merge upstream/main
git push origin main

# Rebase feature branch
git checkout hext/gcp-lifecycle-reporting
git rebase main
git push --force-with-lease origin hext/gcp-lifecycle-reporting
```
