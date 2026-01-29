# Container Persistence Design

## Problem Statement

Currently, workspaces lose user environment state on stop/start because:
1. `docker run --rm` destroys the container when it exits
2. Only `/workspaces` is mounted from persistent storage
3. Home directory changes (`.claude`, `.ssh`, etc.) are lost
4. Installed packages (`apt install`, `pip install`) are lost

The current `persist-home.sh` approach only symlinks items that exist at build time - new dotfiles created later are not persisted.

## Goals

1. **Stop/Start preserves ALL state** - home directory, packages, configs
2. **Rebuild from scratch still works** - explicit rebuild clears everything
3. **Fresh environment variables on restart** - tokens change each start
4. **No manual whitelist maintenance** - new dotfiles auto-persist

## Proposed Solution: Container Persistence

Instead of destroying the container on stop, keep it around and restart it.

### Architecture

```
First Boot:
  VM Start → docker run --name <container> envbuilder → Build → Init

Restart:
  VM Start → docker start <container> → Envbuilder runs again → Skip build → Init

Rebuild:
  VM Start → docker rm <container> → docker run --name <container> → Full build
```

## Implementation Plan

### Phase 1: Template Changes (No Envbuilder Modifications)

**File: `scripts/vm/run-envbuilder.sh`**

```bash
CONTAINER_NAME="coder-${workspace_name}"
ENV_FILE="/home/${linux_user}/env.txt"
CONTAINER_ENV_FILE="/workspaces/.envbuilder-env"

# Copy env to persistent location (for restarts)
cp "$ENV_FILE" "$CONTAINER_ENV_FILE"

# Check if container exists
if docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
  echo "Container exists, starting..."
  docker start "$CONTAINER_NAME"
  # Wait for container to exit (envbuilder will exec into init)
  docker wait "$CONTAINER_NAME"
else
  echo "Creating new container..."
  docker run \
    --name "$CONTAINER_NAME" \
    --net=host \
    -h ${workspace_name} \
    -v /home/${linux_user}/envbuilder:/workspaces \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$CONTAINER_ENV_FILE":/workspaces/.envbuilder-env:ro \
    ${ssh_agent_mount} \
    --env-file "$ENV_FILE" \
    "$image"
fi
```

**Challenge:** Container restart uses original env vars, not fresh ones.

### Phase 2: Envbuilder - Fresh Env on Restart

Add new option to envbuilder:

```go
// options/options.go
{
    Flag:  "env-file",
    Env:   WithEnvPrefix("ENV_FILE"),
    Value: serpent.StringOf(&o.EnvFile),
    Description: "Path to environment file to source at startup. " +
        "This is read on every run, allowing fresh env vars on container restart.",
},
```

**Behavior:**
1. At startup, before any other processing, check if `ENVBUILDER_ENV_FILE` is set
2. If set and file exists, source it (override current env vars)
3. This allows fresh CODER_AGENT_TOKEN etc. on restart

**File: `envbuilder.go` (early in `run()`):**

```go
// Load fresh environment from file if specified
// This enables container restart with new tokens
if opts.EnvFile != "" {
    if err := loadEnvFile(opts.Filesystem, opts.EnvFile); err != nil {
        opts.Logger(log.LevelWarn, "Failed to load env file %s: %v", opts.EnvFile, err)
    } else {
        opts.Logger(log.LevelInfo, "Loaded fresh environment from %s", opts.EnvFile)
    }
}
```

### Phase 3: Explicit Rebuild Mechanism

**Template Parameter:**
```hcl
data "coder_parameter" "force_rebuild" {
  name         = "force_rebuild"
  display_name = "Force Rebuild"
  description  = "Delete container and rebuild from scratch. Use after devcontainer.json changes."
  type         = "bool"
  default      = "false"
  mutable      = true
  ephemeral    = true  # Resets after each build
  order        = 30
}
```

**Startup Script:**
```bash
if [ "${force_rebuild}" = "true" ]; then
  echo "Force rebuild requested, removing existing container..."
  docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
fi
```

### Phase 4: Caching Wins (Future Enhancement)

With container persistence, we could add:

1. **Commit on graceful stop** - Save runtime state to image
   ```bash
   # In a shutdown hook
   docker commit "$CONTAINER_NAME" "${cache_repo}:${workspace_id}-runtime"
   ```

2. **Use runtime image on next start** - Faster than rebuilding
   ```bash
   # Check for runtime cache
   if docker pull "${cache_repo}:${workspace_id}-runtime" 2>/dev/null; then
     builder_image="${cache_repo}:${workspace_id}-runtime"
   fi
   ```

## Migration Path

1. **v0.2.0**: Add `ENVBUILDER_ENV_FILE` support
2. **Template update**: Remove `--rm`, add container naming and lifecycle
3. **v0.3.0** (optional): Add commit-on-stop for runtime caching

## Backward Compatibility

- `ENVBUILDER_ENV_FILE` is optional - existing deployments work unchanged
- Template changes are opt-in per workspace
- `ENVBUILDER_SKIP_REBUILD=true` should be set for container persistence to skip build phase

## Testing Plan

1. Create workspace, install `claude` (creates `~/.claude`)
2. Stop workspace
3. Start workspace
4. Verify `~/.claude` exists with all contents
5. Verify fresh CODER_AGENT_TOKEN works
6. Test explicit rebuild clears everything

## Open Questions

1. **Container cleanup on delete** - Need to ensure container is removed when workspace is deleted
2. **Disk space** - Container filesystems can grow; may need periodic cleanup
3. **Image updates** - How to handle base image updates? Force rebuild?
