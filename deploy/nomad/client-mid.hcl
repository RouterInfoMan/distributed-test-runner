# mid-pool worker: same container abstraction as high-perf-pool, on a smaller
# machine. Together they are what the virtual "global" pool spans.
datacenter = "dc1"
data_dir   = "/tmp/dtp-nomad/mid"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-mid-1"

server { enabled = false }

client {
  enabled    = true
  servers    = ["nomad-server:4647"]
  network_interface = "eth0"

  # 4 slots x the node's 2-core/4096MB slot (node.yaml): host cores 12-19.
  reservable_cores = "12-19"
  memory_total_mb  = 16384

  # Labels (meta.dtp.*), the slot count and the slot size are set at runtime
  # by dtp-node (deploy/nodes/*.yaml) as dynamic node metadata; only the slot
  # count stays here as a fallback for the moment before the agent reports in.
  # No node_pool: the pool this node serves is a row in the master's catalog.
  meta {
    "dtp.slots" = "4"
  }

  host_volume "dtp-cache" {
    path      = "/tmp/dtp-nomad/cache"
    read_only = false
  }
}

plugin "docker" {
  config {
    # Suite containers need the build cache bind-mount.
    volumes { enabled = true }
    allow_privileged = false
    gc {
      image = false
      # Both container clients in this demo share one docker daemon. The
      # driver's dangling-container reaper would treat the other client's
      # suite containers as untracked and SIGKILL them after ~5 minutes.
      # Nodes with their own daemon can leave this at the default.
      dangling_containers { enabled = false }
    }
  }
}

advertise {
  http = "nomad-client-mid"
  rpc  = "nomad-client-mid"
  serf = "nomad-client-mid"
}
