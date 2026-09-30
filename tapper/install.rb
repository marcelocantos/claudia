bin.install "claudia" => "claudia"
# 🎯T157: the Bun sidecar that runs plan seats. The broker finds it beside
# its binary (share/claudia/sidecar) and installs its dependencies there on
# first start.
(share/"claudia").install "sidecar"
