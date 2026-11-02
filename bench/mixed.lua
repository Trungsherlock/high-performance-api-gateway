-- Deterministic per-worker 80% hot / 20% unique key sequence.
local sequence = 0
local worker = 0
setup = function(thread)
  worker = worker + 1
  thread:set("worker_id", worker)
end
request = function()
  sequence = sequence + 1
  local id = "42"
  if sequence % 5 == 0 then id = tostring(worker_id) .. "-" .. tostring(sequence) end
  return wrk.format("GET", "/api/public/item?id=" .. id)
end
