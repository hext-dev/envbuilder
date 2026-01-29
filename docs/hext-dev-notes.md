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

**Current dev version:** `hext-dev-v0.1.3` (GCP instance identity auth + lifecycle reporting + container persistence)

This separation allows easy rollback:
```terraform
# Use stable version
devcontainer_builder_image = "ghcr.io/hext-dev/envbuilder:latest"

# Use dev version with all hext-dev features
devcontainer_builder_image = "ghcr.io/hext-dev/envbuilder:hext-dev-v0.1.3"
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
4. ✅ Update GitHub Actions to build `hext-dev-*` tags

### Testing

```bash
# Template uses new image
devcontainer_builder_image = "ghcr.io/hext-dev/envbuilder:hext-dev-v0.1.3"

# Pass new env vars
"ENVBUILDER_CODER_AUTH_METHOD": "gcp-instance-identity",
"ENVBUILDER_GCP_SERVICE_ACCOUNT": var.service_account_email,
"CODER_AGENT_URL": data.coder_workspace.me.access_url,
```

## Feature: Container Persistence (ENVBUILDER_ENV_FILE)

**Branch:** `hext/gcp-lifecycle-reporting`
**Version:** `hext-dev-v0.1.3`

**Problem:** By default, Coder workspaces use `docker run --rm`, destroying the container on stop. This loses:
- Installed packages (`apt install`, `pip install`)
- Home directory changes (`.claude`, `.ssh`, new dotfiles)
- System configurations

The old workaround was `persist-home.sh` which symlinked specific dotfiles, but this was fragile and couldn't persist packages or new dotfiles created after initial setup.

**Solution:** Container persistence with fresh environment loading:

1. **Template changes:** Don't use `--rm`, reuse existing containers on restart
2. **Envbuilder change:** Add `ENVBUILDER_ENV_FILE` to load fresh env vars on restart

When a container restarts, it runs with the original environment variables from creation. But `CODER_AGENT_TOKEN` changes on each workspace start. `ENVBUILDER_ENV_FILE` solves this by reading fresh env vars from a mounted file.

### New Environment Variable

| Variable | Description |
|----------|-------------|
| `ENVBUILDER_ENV_FILE` | Path to env file to load at startup. Enables fresh tokens on container restart. |

### How It Works

1. Template writes fresh env vars to `/workspaces/.envbuilder-env` on each VM start
2. Envbuilder reads this file at startup, overriding stale container env vars
3. Container state persists, but tokens are always fresh

### Template Integration

```hcl
# Enable container persistence
data "coder_parameter" "enable_container_persist" {
  name    = "enable_container_persist"
  type    = "bool"
  default = "true"
}

# Pass env file path to envbuilder
envbuilder_env = {
  "ENVBUILDER_ENV_FILE" : "/workspaces/.envbuilder-env",
  "ENVBUILDER_SKIP_REBUILD" : "true",
  ...
}
```

See [container-persistence.md](container-persistence.md) for full design details.

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

## Critical Gotchas

### Use Annotated Tags for Releases

**CRITICAL:** The release workflow requires **annotated tags**. Lightweight tags produce wrong versions.

```bash
# WRONG - lightweight tag, produces "hext-dev-v0.1.2+dev-abc123"
git tag hext-dev-v0.1.2
git push origin hext-dev-v0.1.2

# CORRECT - annotated tag, produces "hext-dev-v0.1.2"
git tag -a hext-dev-v0.1.2 -m "Release hext-dev-v0.1.2: GCP lifecycle reporting"
git push origin hext-dev-v0.1.2
```

**Why:** The `scripts/version.sh` uses `git describe --always` to verify the tag. With lightweight tags:
- `git describe --always` returns the commit hash (e.g., `abc123`)
- The script compares `hext-dev-v0.1.2` != `abc123`, fails the check
- Falls back to dev versioning: `hext-dev-v0.1.2+dev-abc123`

With annotated tags, `git describe --always` returns the tag name, and the comparison succeeds.

### Re-pushing Tags

If you need to recreate a tag (e.g., to fix lightweight → annotated):

```bash
# Delete local and remote tag
git tag -d hext-dev-v0.1.2
git push origin --delete hext-dev-v0.1.2

# Create annotated tag and push
git tag -a hext-dev-v0.1.2 -m "Release message"
git push origin hext-dev-v0.1.2
```

### Verifying the Build

After pushing a tag, verify the build pushed the correct image:

```bash
# Watch the build
gh run watch <run-id> -R hext-dev/envbuilder --exit-status

# Check the pushed manifest (should NOT have +dev suffix)
gh run view <run-id> -R hext-dev/envbuilder --log | grep "pushing manifest"
# Should show: ghcr.io/hext-dev/envbuilder:hext-dev-v0.1.2 (no +dev)
```

### Testing the Image

After build completes, test with a new workspace (increment the name each time):

```bash
coder create test-repo-N --template hext-devcontainer \
  --parameter source=https://github.com/hext-dev/test-repo#broken-devcontainer \
  --parameter region=us-east4-a \
  --parameter instance_type=e2-small \
  --parameter fallback_image=none \
  --yes
```

Check the VM logs to verify GCP auth and lifecycle reporting:
```bash
gcloud compute ssh <vm-name> --zone=us-east4-a --command \
  "sudo journalctl -u google-startup-scripts.service | grep -iE 'auth|lifecycle|error'"
```

Expected output for successful lifecycle reporting:
```
Authenticating to Coder using GCP instance identity
Successfully authenticated to Coder via GCP instance identity
Reporting start_error lifecycle state to Coder...
Successfully reported start_error to Coder
```
