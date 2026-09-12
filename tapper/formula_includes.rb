# Claudia is one long-lived host daemon. The formula ships a service
# definition so `brew services start claudia` starts the broker on login
# (🎯T2.7). std_service_path_env FIRST: the daemon spawns provider CLIs
# Homebrew does not package (claude, grok, cursor, codex), so those
# per-user dirs stay on PATH — but behind the system path, or anything
# planted there would win for a background service. TERM/LANG keep
# Claude's TUI from painting blank under launchd.
service do
  run [opt_bin/"claudia", "broker", "serve"]
  environment_variables PATH: [
    std_service_path_env,
    "#{Dir.home}/go/bin",
    "#{Dir.home}/.local/bin",
    "#{Dir.home}/.grok/bin",
  ].join(":"), TERM: "xterm-256color", LANG: "en_US.UTF-8"
  keep_alive true
  log_path var/"log/claudia/broker.log"
  error_log_path var/"log/claudia/broker.log"
end
