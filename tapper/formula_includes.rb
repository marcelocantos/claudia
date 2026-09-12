# Claudia is one long-lived host daemon. The formula ships a service
# definition so `brew services start claudia` starts the broker on login
# (🎯T2.7). PATH and TERM match the owner-installed launchd agent: without
# them provider binaries do not resolve and Claude's TUI paints blank.
service do
  run [opt_bin/"claudia", "broker", "serve"]
  environment_variables PATH: "#{HOMEBREW_PREFIX}/bin:#{HOMEBREW_PREFIX}/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:#{Dir.home}/go/bin:#{Dir.home}/.local/bin:#{Dir.home}/.grok/bin", TERM: "xterm-256color"
  keep_alive true
  log_path var/"log/claudia/broker.log"
  error_log_path var/"log/claudia/broker.log"
end
