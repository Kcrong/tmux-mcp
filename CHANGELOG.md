# Changelog

## [0.2.0](https://github.com/Kcrong/tmux-mcp/compare/v0.1.0...v0.2.0) (2026-05-25)


### Features

* -socket flag for systemd / container deployments ([#24](https://github.com/Kcrong/tmux-mcp/issues/24)) ([1230b06](https://github.com/Kcrong/tmux-mcp/commit/1230b06e5d599d4632e2222763979d52c3f41ae3))
* **capture:** bound scrollback output with max_lines + 5000 default ([#25](https://github.com/Kcrong/tmux-mcp/issues/25)) ([2dbc20c](https://github.com/Kcrong/tmux-mcp/commit/2dbc20c4a9d19f9e8ed97b18f55f33cf5afcac75))
* **cli:** add -dry-run to validate config without serving stdio ([#73](https://github.com/Kcrong/tmux-mcp/issues/73)) ([c6da45a](https://github.com/Kcrong/tmux-mcp/commit/c6da45af0406f19111fcde8a70e6d40bbb002cde))
* **cli:** add -log-format flag to pin slog output shape ([#57](https://github.com/Kcrong/tmux-mcp/issues/57)) ([520345c](https://github.com/Kcrong/tmux-mcp/commit/520345c256d0a8b47977ad6ed4a823fb0c362087))
* **cli:** add -log-output to redirect slog stream to a file ([#77](https://github.com/Kcrong/tmux-mcp/issues/77)) ([9539062](https://github.com/Kcrong/tmux-mcp/commit/95390624ae76eb618cd30dd46f288f65e9e6a46b))
* **cli:** add -log-source flag to include source location in logs ([#66](https://github.com/Kcrong/tmux-mcp/issues/66)) ([7b7cfcd](https://github.com/Kcrong/tmux-mcp/commit/7b7cfcd1a67046336bd4067dd049695ee7ff6bad))
* **cli:** add -pid-file to externalise the server PID ([#81](https://github.com/Kcrong/tmux-mcp/issues/81)) ([c149d82](https://github.com/Kcrong/tmux-mcp/commit/c149d82547686c569f23b3c799453c54d1de69f6))
* **cli:** add -probe flag for startup health check ([#43](https://github.com/Kcrong/tmux-mcp/issues/43)) ([270d96c](https://github.com/Kcrong/tmux-mcp/commit/270d96ce456ded3ccedcf2b24d7e105ed51bb9ff))
* **cli:** add -tmux-bin to pin the tmux binary path ([#93](https://github.com/Kcrong/tmux-mcp/issues/93)) ([5914ff6](https://github.com/Kcrong/tmux-mcp/commit/5914ff6fcf9d769d9b0d3d7bf5f0fedba6966790))
* **cli:** add -tmux-config-path to load a custom tmux.conf ([#113](https://github.com/Kcrong/tmux-mcp/issues/113)) ([b9b153a](https://github.com/Kcrong/tmux-mcp/commit/b9b153ae9579ee022b9415cad9240955dbe6defc))
* **cli:** add -version-json flag for machine-readable metadata ([#44](https://github.com/Kcrong/tmux-mcp/issues/44)) ([cbe74c1](https://github.com/Kcrong/tmux-mcp/commit/cbe74c1b355270c06ebef001107a546eaa7d75de))
* **errs:** typed errors with stable JSON-RPC codes ([#31](https://github.com/Kcrong/tmux-mcp/issues/31)) ([d2ab1f6](https://github.com/Kcrong/tmux-mcp/commit/d2ab1f623aec448c13c726bbb6a107953a730d29))
* **release:** publish OCI image to ghcr.io/kcrong/tmux-mcp ([#36](https://github.com/Kcrong/tmux-mcp/issues/36)) ([cfe46f7](https://github.com/Kcrong/tmux-mcp/commit/cfe46f74da607d87bd93f4b28b3adaf07545d899))
* **server:** add -allowlist to gate exposed tools ([#84](https://github.com/Kcrong/tmux-mcp/issues/84)) ([40d06b3](https://github.com/Kcrong/tmux-mcp/commit/40d06b38a762e8541f24761adc283f5dcee62fea))
* **server:** add -log-rotate-size and -log-rotate-keep for log file rotation ([#102](https://github.com/Kcrong/tmux-mcp/issues/102)) ([72e2913](https://github.com/Kcrong/tmux-mcp/commit/72e2913f0b0efd0fca873b47ca9a8ea3b6828fb1))
* **server:** add -max-response-bytes to cap JSON-RPC response size ([#95](https://github.com/Kcrong/tmux-mcp/issues/95)) ([d03a457](https://github.com/Kcrong/tmux-mcp/commit/d03a457b7e7bd5f6a27436176d4b076908d82284))
* **server:** add -pprof to expose pprof on the metrics listener ([#86](https://github.com/Kcrong/tmux-mcp/issues/86)) ([bc5f7fe](https://github.com/Kcrong/tmux-mcp/commit/bc5f7fe682bd62cbb728dad6391ad161543cf120))
* **server:** add -read-only to gate mutating tools ([#106](https://github.com/Kcrong/tmux-mcp/issues/106)) ([bc80095](https://github.com/Kcrong/tmux-mcp/commit/bc80095ca9bed94e4181c2935379936650a1ed53))
* **server:** add -session-idle-timeout to reap inactive sessions ([#69](https://github.com/Kcrong/tmux-mcp/issues/69)) ([363de8b](https://github.com/Kcrong/tmux-mcp/commit/363de8ba9e18f8871a33e64da2350b3526ba5db4))
* **server:** add -session-prefix to namespace session names ([#94](https://github.com/Kcrong/tmux-mcp/issues/94)) ([0cf01ee](https://github.com/Kcrong/tmux-mcp/commit/0cf01eeeb1165007dce1874d956e395ec3064e02))
* **server:** add -shutdown-timeout for graceful SIGTERM drain ([#62](https://github.com/Kcrong/tmux-mcp/issues/62)) ([6012a19](https://github.com/Kcrong/tmux-mcp/commit/6012a194c41631dcdd1f7db2c565421c5d723718))
* **server:** add optional Prometheus -metrics-addr exporter ([#65](https://github.com/Kcrong/tmux-mcp/issues/65)) ([4216233](https://github.com/Kcrong/tmux-mcp/commit/4216233003ef7b47e165aa2d6cd1a023cbe71573))
* **server:** bound tool inputs to prevent abuse ([#28](https://github.com/Kcrong/tmux-mcp/issues/28)) ([b08eb0e](https://github.com/Kcrong/tmux-mcp/commit/b08eb0e18896a9415d2e643f6ad392635c3701fe))
* **server:** cap concurrent tool calls with -max-concurrent-calls ([#51](https://github.com/Kcrong/tmux-mcp/issues/51)) ([2b99d99](https://github.com/Kcrong/tmux-mcp/commit/2b99d99c419141af753c0fd93a46627c3dde3829))
* **server:** drain in-flight goroutines on shutdown ([#21](https://github.com/Kcrong/tmux-mcp/issues/21)) ([59bb65a](https://github.com/Kcrong/tmux-mcp/commit/59bb65a26bdfe4b5cf938fae280edb504c4f504f))
* **server:** emit notifications/tools/list_changed when tools mutate ([#63](https://github.com/Kcrong/tmux-mcp/issues/63)) ([280db05](https://github.com/Kcrong/tmux-mcp/commit/280db05c20481576e2538599ec8785c5e36184b8))
* **server:** expose /healthz on the metrics listener ([#78](https://github.com/Kcrong/tmux-mcp/issues/78)) ([26b444f](https://github.com/Kcrong/tmux-mcp/commit/26b444f7d31eb07cdc4e52de81f0285f9924ee48))
* **server:** per-request ID for log correlation ([#16](https://github.com/Kcrong/tmux-mcp/issues/16)) ([4c5cc12](https://github.com/Kcrong/tmux-mcp/commit/4c5cc1242eabe52f1d2092ea6f6391026ddfa883))
* **server:** recover from handler panics, prevent shutdown hang ([#34](https://github.com/Kcrong/tmux-mcp/issues/34)) ([dadd28a](https://github.com/Kcrong/tmux-mcp/commit/dadd28a714c4901fa8dae0fada576274987daf85))
* **server:** structured audit log via -audit-log flag ([#53](https://github.com/Kcrong/tmux-mcp/issues/53)) ([bbe5a70](https://github.com/Kcrong/tmux-mcp/commit/bbe5a7007a44fbc722bd03f69167d0fe40bd19b6))
* ship Windows binaries + cross-build smoke test ([#26](https://github.com/Kcrong/tmux-mcp/issues/26)) ([4cfce11](https://github.com/Kcrong/tmux-mcp/commit/4cfce11dd97a69bdcca970751dad14141d7c28b3))
* **snapshot:** add time-based TTL cleanup to bound memory growth ([#60](https://github.com/Kcrong/tmux-mcp/issues/60)) ([a5f7648](https://github.com/Kcrong/tmux-mcp/commit/a5f76485eb95c97f429cac45e8044dc96e82f46b))
* structured logging via slog with --log-level flag ([#11](https://github.com/Kcrong/tmux-mcp/issues/11)) ([9a576df](https://github.com/Kcrong/tmux-mcp/commit/9a576df6292c88744a44700f68d36f9eb22f646e))
* **tmuxctl:** require tmux 3.0+ at startup with a clear error ([#10](https://github.com/Kcrong/tmux-mcp/issues/10)) ([bec6d90](https://github.com/Kcrong/tmux-mcp/commit/bec6d908b461eb5b8e42a748a6c8de4193c504ba))
* **tools:** add choose_client for tmux choose-client ([#165](https://github.com/Kcrong/tmux-mcp/issues/165)) ([0edc4f6](https://github.com/Kcrong/tmux-mcp/commit/0edc4f6f0628ccade24310fab5221dd8ea4b4d1c))
* **tools:** add choose_tree tool ([#108](https://github.com/Kcrong/tmux-mcp/issues/108)) ([e800b69](https://github.com/Kcrong/tmux-mcp/commit/e800b693c10cc2a89e9fb2ac3dfe1be46f9ee22c))
* **tools:** add clear_history for tmux clear-history ([#85](https://github.com/Kcrong/tmux-mcp/issues/85)) ([0a6a35c](https://github.com/Kcrong/tmux-mcp/commit/0a6a35c5fe81a536d3e01679082662e6b6d22fd7))
* **tools:** add clock_mode for tmux clock-mode ([#158](https://github.com/Kcrong/tmux-mcp/issues/158)) ([ec62238](https://github.com/Kcrong/tmux-mcp/commit/ec6223898da73b7ac3067eb22324f887385c75dc))
* **tools:** add cursor pagination to capture for huge scrollback ([#64](https://github.com/Kcrong/tmux-mcp/issues/64)) ([0046517](https://github.com/Kcrong/tmux-mcp/commit/0046517dc2f22522f7882229f824a16287f2f239))
* **tools:** add detach_client for tmux detach-client ([#143](https://github.com/Kcrong/tmux-mcp/issues/143)) ([4bf10b9](https://github.com/Kcrong/tmux-mcp/commit/4bf10b909b18ee7790b5f6e6e90b40aa7cf69a45))
* **tools:** add display_message for tmux format-string introspection ([#99](https://github.com/Kcrong/tmux-mcp/issues/99)) ([a7b05ac](https://github.com/Kcrong/tmux-mcp/commit/a7b05ac4b8f9f7f246e0c5aeecef071f70d2912b))
* **tools:** add display_panes for tmux display-panes ([#167](https://github.com/Kcrong/tmux-mcp/issues/167)) ([09ce3e8](https://github.com/Kcrong/tmux-mcp/commit/09ce3e895b853be3406772089d1d27e5f555e857))
* **tools:** add display_popup for tmux display-popup ([#171](https://github.com/Kcrong/tmux-mcp/issues/171)) ([a4ede52](https://github.com/Kcrong/tmux-mcp/commit/a4ede52ac4f733070ce065caf8b3b2863f4dbf90))
* **tools:** add has_session for tmux has-session ([#107](https://github.com/Kcrong/tmux-mcp/issues/107)) ([55c330b](https://github.com/Kcrong/tmux-mcp/commit/55c330b8162a083d577be14f610d8a6976d0b83c))
* **tools:** add kill_all_sessions for one-shot recovery ([#45](https://github.com/Kcrong/tmux-mcp/issues/45)) ([145fee3](https://github.com/Kcrong/tmux-mcp/commit/145fee385ad8628b1bf21c9691474a3bf40759d5))
* **tools:** add kill_server for tmux kill-server ([#124](https://github.com/Kcrong/tmux-mcp/issues/124)) ([b1cfa06](https://github.com/Kcrong/tmux-mcp/commit/b1cfa0679f86ab7db2c989aa89a823a929e9db16))
* **tools:** add kill_window for tmux kill-window ([#101](https://github.com/Kcrong/tmux-mcp/issues/101)) ([8cdcd45](https://github.com/Kcrong/tmux-mcp/commit/8cdcd45e8cc94537bd28006ebc2538c0950a0b0c))
* **tools:** add last_pane for tmux last-pane ([#166](https://github.com/Kcrong/tmux-mcp/issues/166)) ([be7f62c](https://github.com/Kcrong/tmux-mcp/commit/be7f62cb6dde95444aa39b3a1661f756e8b1662d))
* **tools:** add list_buffers and show_buffer for tmux paste buffers ([#98](https://github.com/Kcrong/tmux-mcp/issues/98)) ([b028da5](https://github.com/Kcrong/tmux-mcp/commit/b028da5ca3f03177f33e9be71e241e3d0ca2243e))
* **tools:** add list_clients for tmux list-clients ([#97](https://github.com/Kcrong/tmux-mcp/issues/97)) ([d994e75](https://github.com/Kcrong/tmux-mcp/commit/d994e758f8d8b651ef050f840a674acd7910120b))
* **tools:** add list_keys for tmux list-keys ([#120](https://github.com/Kcrong/tmux-mcp/issues/120)) ([843731d](https://github.com/Kcrong/tmux-mcp/commit/843731d2383e35e0758245e30cd50af9e86fdb56))
* **tools:** add list_windows to enumerate tmux windows ([#72](https://github.com/Kcrong/tmux-mcp/issues/72)) ([e11bc0d](https://github.com/Kcrong/tmux-mcp/commit/e11bc0dd1203812aa645f8e0d139764b4a1846b7))
* **tools:** add lock_server for tmux lock-server ([#169](https://github.com/Kcrong/tmux-mcp/issues/169)) ([39711b6](https://github.com/Kcrong/tmux-mcp/commit/39711b6ae22adc27549bdd13f1550519543a754c))
* **tools:** add move_pane for tmux move-pane ([#137](https://github.com/Kcrong/tmux-mcp/issues/137)) ([fb335be](https://github.com/Kcrong/tmux-mcp/commit/fb335be1fcb44e3fa48bd0c40b520b0f2fc21396))
* **tools:** add new_window for tmux new-window ([#100](https://github.com/Kcrong/tmux-mcp/issues/100)) ([0814335](https://github.com/Kcrong/tmux-mcp/commit/08143358c9c469b7ceb1c9febccaa1ef008ec18e))
* **tools:** add pane_break for tmux break-pane ([#90](https://github.com/Kcrong/tmux-mcp/issues/90)) ([0eed72f](https://github.com/Kcrong/tmux-mcp/commit/0eed72fb0f87822f8bd5da21c7b8b7eba36c2030))
* **tools:** add pane_join for tmux join-pane ([#92](https://github.com/Kcrong/tmux-mcp/issues/92)) ([ff45c20](https://github.com/Kcrong/tmux-mcp/commit/ff45c20357ecb5022f41075d432d94f96f914baa))
* **tools:** add pane_kill for tmux kill-pane ([#75](https://github.com/Kcrong/tmux-mcp/issues/75)) ([a4c285a](https://github.com/Kcrong/tmux-mcp/commit/a4c285aca81b5ebb0399f0751c62ee2c566f975b))
* **tools:** add pane_resize for tmux resize-pane ([#82](https://github.com/Kcrong/tmux-mcp/issues/82)) ([0f77e76](https://github.com/Kcrong/tmux-mcp/commit/0f77e76f2f5539a5fbaf51c3417a499235d0a55b))
* **tools:** add pane_split for tmux split-window ([#71](https://github.com/Kcrong/tmux-mcp/issues/71)) ([5169f0e](https://github.com/Kcrong/tmux-mcp/commit/5169f0ed0912ab8b59cb163291628d523ebc4666))
* **tools:** add pane_swap for tmux swap-pane ([#79](https://github.com/Kcrong/tmux-mcp/issues/79)) ([d9b2227](https://github.com/Kcrong/tmux-mcp/commit/d9b22278ca83b11cef4d1efa3933d07d4fce65eb))
* **tools:** add respawn_pane for tmux respawn-pane ([#104](https://github.com/Kcrong/tmux-mcp/issues/104)) ([ef852b8](https://github.com/Kcrong/tmux-mcp/commit/ef852b862eccec5663e848db6a7e23b545aae28d))
* **tools:** add run_shell for tmux run-shell ([#157](https://github.com/Kcrong/tmux-mcp/issues/157)) ([4b729f0](https://github.com/Kcrong/tmux-mcp/commit/4b729f01351fe9478f43dcdabe0156a90fda1d15))
* **tools:** add send_signal for precise pane PID signalling ([#59](https://github.com/Kcrong/tmux-mcp/issues/59)) ([30e4729](https://github.com/Kcrong/tmux-mcp/commit/30e4729e131578668d1df4f148fc3096076e678a))
* **tools:** add session_describe for structured session metadata ([#52](https://github.com/Kcrong/tmux-mcp/issues/52)) ([42abe04](https://github.com/Kcrong/tmux-mcp/commit/42abe04da30cb802885b7f5c48dfc82c95c8026c))
* **tools:** add session_inspect for active-pane process state ([#58](https://github.com/Kcrong/tmux-mcp/issues/58)) ([5cb6047](https://github.com/Kcrong/tmux-mcp/commit/5cb6047f50cc318de8f3f94f0b88435993a59f2c))
* **tools:** add session_rename for tmux rename-session ([#76](https://github.com/Kcrong/tmux-mcp/issues/76)) ([6cea2ad](https://github.com/Kcrong/tmux-mcp/commit/6cea2ad98d6d6e2f93a8eac6d2ea24f15c6a2356))
* **tools:** add set_buffer for tmux set-buffer ([#105](https://github.com/Kcrong/tmux-mcp/issues/105)) ([37d5ac2](https://github.com/Kcrong/tmux-mcp/commit/37d5ac2c170c5dc58f7ca58ab54eba408f76c77c))
* **tools:** add set_window_option for tmux set-window-option ([#156](https://github.com/Kcrong/tmux-mcp/issues/156)) ([e361e20](https://github.com/Kcrong/tmux-mcp/commit/e361e201b904999f478e8a7f92258101e9edf66a))
* **tools:** add show_messages for tmux show-messages ([#150](https://github.com/Kcrong/tmux-mcp/issues/150)) ([ec24503](https://github.com/Kcrong/tmux-mcp/commit/ec24503e4dad87af82693c53be4fc1ed4f79fa76))
* **tools:** add show_options for tmux show-options ([#96](https://github.com/Kcrong/tmux-mcp/issues/96)) ([46c9e43](https://github.com/Kcrong/tmux-mcp/commit/46c9e434fd98758b4287a1b236dcfd5b3fbc8892))
* **tools:** add show_window_options for tmux show-window-options ([#154](https://github.com/Kcrong/tmux-mcp/issues/154)) ([b01dec0](https://github.com/Kcrong/tmux-mcp/commit/b01dec03eb5dbf25265c12162ec9f013d96ab984))
* **tools:** add start_server for tmux start-server ([#121](https://github.com/Kcrong/tmux-mcp/issues/121)) ([72041f4](https://github.com/Kcrong/tmux-mcp/commit/72041f433ef6b90aee3c4213bc10998c65765927))
* **tools:** add swap_window for tmux swap-window ([#103](https://github.com/Kcrong/tmux-mcp/issues/103)) ([3a75655](https://github.com/Kcrong/tmux-mcp/commit/3a7565555490c8d83b4e408e725a8fdac5ddad55))
* **tools:** add switch_client for tmux switch-client ([#142](https://github.com/Kcrong/tmux-mcp/issues/142)) ([1676ddc](https://github.com/Kcrong/tmux-mcp/commit/1676ddcecdcdb61897df2f2459eebb613329e6d2))
* **tools:** add unbind_key for tmux unbind-key ([#144](https://github.com/Kcrong/tmux-mcp/issues/144)) ([7cb522a](https://github.com/Kcrong/tmux-mcp/commit/7cb522aa0d9babd4f6f7e308414d8c4fa1403e27))
* **tools:** add window_create and window_kill tools ([#68](https://github.com/Kcrong/tmux-mcp/issues/68)) ([43a69b8](https://github.com/Kcrong/tmux-mcp/commit/43a69b8fd551eea4f26291162335e1fbde370697))
* **tools:** add window_move for tmux move-window ([#80](https://github.com/Kcrong/tmux-mcp/issues/80)) ([533897c](https://github.com/Kcrong/tmux-mcp/commit/533897cf9d353394c9347b084bd94049a3f53510))
* **tools:** add window_select and window_rename ([#74](https://github.com/Kcrong/tmux-mcp/issues/74)) ([b94c217](https://github.com/Kcrong/tmux-mcp/commit/b94c217b995b43dbb444f04e846da18d6f0755c0))
* **tools:** list_panes + pane_select for multi-pane TUIs ([#47](https://github.com/Kcrong/tmux-mcp/issues/47)) ([cca8afb](https://github.com/Kcrong/tmux-mcp/commit/cca8afb0dd49cba052e08b40051f7c19fe5143cc))


### Bug Fixes

* **server:** cancel in-flight calls when client closes stdin ([#67](https://github.com/Kcrong/tmux-mcp/issues/67)) ([c294361](https://github.com/Kcrong/tmux-mcp/commit/c294361308a0d0605021cf8a3036ea6f549f5b82))
* **server:** expose binary version in initialize response ([#22](https://github.com/Kcrong/tmux-mcp/issues/22)) ([1f088dc](https://github.com/Kcrong/tmux-mcp/commit/1f088dc65524d6ee61a8c403f082fa8458448f2c))
* **snapshot:** clean up per-session history on session_kill ([#19](https://github.com/Kcrong/tmux-mcp/issues/19)) ([679880b](https://github.com/Kcrong/tmux-mcp/commit/679880bb62a6530189964499fb5943d1d735d89f))


### Performance

* **tmuxctl:** compile wait_for_text regex once, not per poll ([#20](https://github.com/Kcrong/tmux-mcp/issues/20)) ([42a3ca2](https://github.com/Kcrong/tmux-mcp/commit/42a3ca2851287f0ee2f60d39c076f4208c4c215b))


### CI

* add actionlint + gosec lint-actions workflow ([#70](https://github.com/Kcrong/tmux-mcp/issues/70)) ([436840f](https://github.com/Kcrong/tmux-mcp/commit/436840f1e46f8c0d6158c8e1d8b45d99bb538a40))
* automate releases with release-please + conventional commits ([#29](https://github.com/Kcrong/tmux-mcp/issues/29)) ([3838d6d](https://github.com/Kcrong/tmux-mcp/commit/3838d6d06450c1368a860eddaf8116ae014b6192))
* cancel in-progress runs and cache lint deps ([#18](https://github.com/Kcrong/tmux-mcp/issues/18)) ([73ad0f0](https://github.com/Kcrong/tmux-mcp/commit/73ad0f0f5f25699bd65fa85be3f9a9aebe877dd3))
* enable shadow / gofumpt / unconvert / misspell linters ([#32](https://github.com/Kcrong/tmux-mcp/issues/32)) ([5de37b4](https://github.com/Kcrong/tmux-mcp/commit/5de37b4f125449ec675aea35947f2960cfab1a42))
* go install smoke test + Codecov coverage threshold ([#27](https://github.com/Kcrong/tmux-mcp/issues/27)) ([8aa8d36](https://github.com/Kcrong/tmux-mcp/commit/8aa8d36798d3d9f3c841a1e4dc8f46c9edad90b9))
* pin all GitHub Actions to 40-char commit SHA ([#23](https://github.com/Kcrong/tmux-mcp/issues/23)) ([f9411df](https://github.com/Kcrong/tmux-mcp/commit/f9411df81629a3040841414ac9d635afec25624a))
* **release:** SBOMs + cosign keyless signatures ([#15](https://github.com/Kcrong/tmux-mcp/issues/15)) ([460de6e](https://github.com/Kcrong/tmux-mcp/commit/460de6e10c23aa629841245057244dc8185f0c29))
* reproducible builds via CommitDate + mod_timestamp ([#33](https://github.com/Kcrong/tmux-mcp/issues/33)) ([d5da5fd](https://github.com/Kcrong/tmux-mcp/commit/d5da5fdc8c8e429541d9515fc179cad4ad078299))
* split govulncheck into its own workflow with weekly cron ([#40](https://github.com/Kcrong/tmux-mcp/issues/40)) ([9702945](https://github.com/Kcrong/tmux-mcp/commit/9702945e54c77d485064581512b3d5358d1981e1))
* test on macos-latest matrix leg ([#49](https://github.com/Kcrong/tmux-mcp/issues/49)) ([5bdace3](https://github.com/Kcrong/tmux-mcp/commit/5bdace3d7c867a4e91a0baecca1497e9c14d237e))
* upload coverage to Codecov + add badge ([#13](https://github.com/Kcrong/tmux-mcp/issues/13)) ([468bc8f](https://github.com/Kcrong/tmux-mcp/commit/468bc8fdff44dff8d5d882f445d5f3e089dd35eb))


### Documentation

* 60-second Quickstart + CodeQL badge ([#12](https://github.com/Kcrong/tmux-mcp/issues/12)) ([4e8097b](https://github.com/Kcrong/tmux-mcp/commit/4e8097b44ffc33ae79dc4bf22ab27f1d7f223da3))
* add docs/tools.md and docs/flags.md catalogs ([#56](https://github.com/Kcrong/tmux-mcp/issues/56)) ([8bd0f08](https://github.com/Kcrong/tmux-mcp/commit/8bd0f088a501cf06f31c802575606a8bb991d3dc))
* add godoc comments to all exported symbols ([#38](https://github.com/Kcrong/tmux-mcp/issues/38)) ([75201b3](https://github.com/Kcrong/tmux-mcp/commit/75201b300e9532b28797ada35a7ecfb2d68d895a))
* add Performance & tuning section to README ([#54](https://github.com/Kcrong/tmux-mcp/issues/54)) ([c5f4f83](https://github.com/Kcrong/tmux-mcp/commit/c5f4f83a31b99c871e750ee05cca9063ae00e5ea))
* add Troubleshooting section to README ([#61](https://github.com/Kcrong/tmux-mcp/issues/61)) ([7d7eec8](https://github.com/Kcrong/tmux-mcp/commit/7d7eec80c35e8a186d0cc64bda113c54222abb35))
* ASCII architecture diagram + FAQ section ([#30](https://github.com/Kcrong/tmux-mcp/issues/30)) ([0db496f](https://github.com/Kcrong/tmux-mcp/commit/0db496fcb26ba80c3a2441e4bf02aa722902eaa6))
* copy-paste MCP client config examples ([#14](https://github.com/Kcrong/tmux-mcp/issues/14)) ([7ffd5c6](https://github.com/Kcrong/tmux-mcp/commit/7ffd5c6f854004f0ac0a139f94df4854131a6a14))
* **deploy:** add scripts/tmux-mcp.service systemd unit reference ([#50](https://github.com/Kcrong/tmux-mcp/issues/50)) ([46f60b4](https://github.com/Kcrong/tmux-mcp/commit/46f60b4adbcd1e04522c7d3a8ad9fed37902b453))


### Tests

* enable t.Parallel() on pure-Go tests for CI speedup ([#48](https://github.com/Kcrong/tmux-mcp/issues/48)) ([25e819f](https://github.com/Kcrong/tmux-mcp/commit/25e819fa8bc6cc355a4f2bd2f18259a6835ed235))
* fuzz snapshot diff and wait_for_text regex ([#17](https://github.com/Kcrong/tmux-mcp/issues/17)) ([034ed6b](https://github.com/Kcrong/tmux-mcp/commit/034ed6b57a26a880e371a81450f7f4147cae4183))
* hot-path benchmarks + benchstat CI comparison ([#37](https://github.com/Kcrong/tmux-mcp/issues/37)) ([2f88261](https://github.com/Kcrong/tmux-mcp/commit/2f88261cd137ae8e75689a536514a9a99932187e))
* **stress:** add opt-in stress workflow + leak/race harness ([#55](https://github.com/Kcrong/tmux-mcp/issues/55)) ([18a64da](https://github.com/Kcrong/tmux-mcp/commit/18a64da443822ea4824dcb76b79bd7c63ab5ab99))

## Changelog
