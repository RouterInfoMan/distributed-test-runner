# high-perf-pool worker: suites run as OCI containers via the docker driver.
# meta.dtp.* is what the master constrains on, and meta.dtp.slots declares how
# many suites this node runs in parallel.
datacenter = "dc1"
# Identical to the host path bind-mounted in compose: the docker daemon
# bind-mounts alloc dirs into suite containers, so the path must resolve on
# the host exactly as it does inside this client.
data_dir   = "/tmp/dtp-nomad/hp"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-hp-1"

server { enabled = false }

client {
  enabled    = true
  servers    = ["nomad-server:4647"]
  network_interface = "eth0"

  # 6 slots x the node's 2-core/4096MB slot (node.yaml): this client owns host cores 0-11
  # (the other demo clients take other ranges; a real node owns them all).
  # Tasks may burst to the slot's memory_max on top of the reservation.
  reservable_cores = "0-11"
  memory_total_mb  = 24576

  # Labels (meta.dtp.*), the slot count and the slot size are set at runtime
  # by dtp-node (deploy/nodes/*.yaml) as dynamic node metadata; only the slot
  # count stays here as a fallback for the moment before the agent reports in.
  # No node_pool: the pool this node serves is a row in the master's catalog.
  meta {
    "dtp.slots" = "6"
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
  http = "nomad-client-hp"
  rpc  = "nomad-client-hp"
  serf = "nomad-client-hp"
}
