-- Read-only: every Rex terminal block with its ids, pids and cwd.
-- Run as `rex --autostart=false do - --json`, this script on stdin.
local T = "com.superlogical.terminal"
local out = {}
for _, s in ipairs(rex.call("session.list").sessions) do
  rex.session.attach{ session_id = s.session_id } -- else 409 "stream is not attached"
  for _, b in ipairs(rex.call("session.list_blocks", { session_id = s.session_id }).blocks) do
    if b.creator_name == T then
      local p = rex.call("block.method", {
        session_id = s.session_id, block_id = b.block_id, method = "process", args = {},
      })
      table.insert(out, {
        session_id = s.session_id, label = s.label, window_id = b.window_id, block_id = b.block_id,
        fg = p.foreground and p.foreground.pid, child = p.child and p.child.pid,
        cwd = p.foreground and p.foreground.cwd,
      })
    end
  end
end
return out
