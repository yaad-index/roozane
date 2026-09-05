# Changelog

## [0.2.0](https://github.com/yaad-index/roozane/compare/v0.1.0...v0.2.0) (2026-09-05)


### Features

* enforce the retention windows instead of only validating them ([#30](https://github.com/yaad-index/roozane/issues/30)) ([fe94647](https://github.com/yaad-index/roozane/commit/fe946473583161cbb773b3173d75802f072cc65c))
* record empty collector runs so cadence survives a quiet source ([#26](https://github.com/yaad-index/roozane/issues/26)) ([5ad6115](https://github.com/yaad-index/roozane/commit/5ad6115fc45488cf5bcbef41bd0f0f50a244a330))
* reject a retention window shorter than the longest cadence ([#28](https://github.com/yaad-index/roozane/issues/28)) ([64256c4](https://github.com/yaad-index/roozane/commit/64256c485cb93c82d9b8a833eba8190eafb4748e))

## 0.1.0 (2026-09-04)


### Features

* aggregator with per-item judgement and digest assembly ([#20](https://github.com/yaad-index/roozane/issues/20)) ([6a93ff2](https://github.com/yaad-index/roozane/commit/6a93ff23b1142355ea2360f48ddb4824eec34a60))
* collector core with feed, http and inbox drain ([#17](https://github.com/yaad-index/roozane/issues/17)) ([139b59a](https://github.com/yaad-index/roozane/commit/139b59a0748fa28578078d519ad4645bcc9be111))
* config schema for sources, cadence, and aggregator settings ([#12](https://github.com/yaad-index/roozane/issues/12)) ([c71dddf](https://github.com/yaad-index/roozane/commit/c71dddf6cf6e0a71729618f483b747885f48a90d))
* sink layer with file, chat and external deliveries ([#21](https://github.com/yaad-index/roozane/issues/21)) ([576c528](https://github.com/yaad-index/roozane/commit/576c528132b8e862154f758b7ee200ecf75c015b))
* sinks section and per-entry env allow-list ([#16](https://github.com/yaad-index/roozane/issues/16)) ([bef5f92](https://github.com/yaad-index/roozane/commit/bef5f92129a5e6bca409a179eb564423eab61604))
