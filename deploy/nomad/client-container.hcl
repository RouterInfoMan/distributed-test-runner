# Worker node for the "container" abstraction: suites run as OCI containers via
# the docker driver. meta.dtp.* is what the master constrains on, and
# meta.dtp.slots declares how many suites this node runs in parallel.
datacenter = "dc1"
# Identical to the host path bind-mounted in compose: the docker daemon
# bind-mounts alloc dirs into suite containers, so the path must resolve on
# the host exactly as it does inside this client.
data_dir   = "/tmp/dtp-nomad/container"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-container-1"

server { enabled = false }

client {
  enabled    = true
  node_pool  = "linux-container"
  servers    = ["nomad-server:4647"]
  network_interface = "eth0"

  # 3 slots x the pool's 500MHz/512MB slot size.
  cpu_total_compute = 1500
  memory_total_mb   = 1536

  meta {
    "dtp.slots"       = "3"
    "dtp.os"          = "linux"
    "dtp.display"     = "xvfb"
    "dtp.rcp_version" = "4.30"
    "dtp.arch"        = "x86_64"
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
    gc { image = false }
  }
}

advertise {
  http = "nomad-client-container"
  rpc  = "nomad-client-container"
  serf = "nomad-client-container"
}
