# A second, smaller dev-pool node. Any node runs any product version: the
# version is a property of the build payload, not of the machine.
datacenter = "dc1"
data_dir   = "/nomad/data/dev-2"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-dev-2"

server { enabled = false }

client {
  enabled   = true
  servers   = ["nomad-server:4647"]
  network_interface = "eth0"

  # 1 slot x 2 cores / 4096MB (see client-dev.hcl about the core range).
  reservable_cores = "4-5"
  memory_total_mb  = 4096

  # Labels (meta.dtp.*), the slot count and the slot size are set at runtime
  # by dtp-node (deploy/nodes/*.yaml) as dynamic node metadata; only the slot
  # count stays here as a fallback for the moment before the agent reports in.
  # No node_pool: the pool this node serves is a row in the master's catalog.
  meta {
    "dtp.slots" = "1"
  }
}

plugin "raw_exec" {
  config { enabled = true }
}

advertise {
  http = "nomad-client-dev-2"
  rpc  = "nomad-client-dev-2"
  serf = "nomad-client-dev-2"
}
