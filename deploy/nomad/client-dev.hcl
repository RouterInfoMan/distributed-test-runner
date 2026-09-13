# dev-pool worker: the "process" abstraction. Nomad gives each suite its own
# task directory and process cgroup (exec driver in production; raw_exec here
# because the client itself runs inside a container).
datacenter = "dc1"
data_dir   = "/nomad/data/dev"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-dev-1"

server { enabled = false }

client {
  enabled   = true
  servers   = ["nomad-server:4647"]

  # Pinned so the fingerprinter does not shell out to /sbin/ip, which a
  # minimal worker image need not carry.
  network_interface = "eth0"

  # 2 slots x the node's 2-core/4096MB slot (node.yaml). The demo runs four clients on
  # one host, so this range overlaps the container clients' cores; a real
  # dev box owns all of its cores and needs no reservable_cores at all.
  reservable_cores = "0-3"
  memory_total_mb  = 8192

  # Labels (meta.dtp.*), the slot count and the slot size are set at runtime
  # by dtp-node (deploy/nodes/*.yaml) as dynamic node metadata; only the slot
  # count stays here as a fallback for the moment before the agent reports in.
  # No node_pool: the pool this node serves is a row in the master's catalog.
  meta {
    "dtp.slots" = "2"
  }
}

plugin "raw_exec" {
  config { enabled = true }
}

advertise {
  http = "nomad-client-dev"
  rpc  = "nomad-client-dev"
  serf = "nomad-client-dev"
}
