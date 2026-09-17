# Changelog

## [0.8.0](https://github.com/yaad-index/roozane/compare/v0.7.0...v0.8.0) (2026-09-17)


### Features

* record what the title pass did, not only that it ran ([#25](https://github.com/yaad-index/roozane/issues/25)) ([79b15c5](https://github.com/yaad-index/roozane/commit/79b15c5ad7355fee126758a0de8783bce8d1f4d8))


### Bug Fixes

* don't report a legacy-path digest as the aggregator not running ([#18](https://github.com/yaad-index/roozane/issues/18)) ([6332705](https://github.com/yaad-index/roozane/commit/6332705930bf56a059315edfef049a09dca1f0a7))
* stop drawing the unlabelled-cluster placeholder from the label namespace ([#23](https://github.com/yaad-index/roozane/issues/23)) ([7636825](https://github.com/yaad-index/roozane/commit/76368250ee814066746d07bb58137e7baeaf0f9c))

## [0.7.0](https://github.com/yaad-index/roozane/compare/v0.6.0...v0.7.0) (2026-09-09)


### Features

* **deliver:** hand exec sinks the written prose, not only the structured digest ([#84](https://github.com/yaad-index/roozane/issues/84)) ([00efc20](https://github.com/yaad-index/roozane/commit/00efc206a470cc556f01f6899164a5e379b747c3))
* fill a digest to a length subject by subject, not a cap then a ceiling ([#86](https://github.com/yaad-index/roozane/issues/86)) ([eb08ede](https://github.com/yaad-index/roozane/commit/eb08ede652753ff28046125a8b20a8803fea7b9d))

## [0.6.0](https://github.com/yaad-index/roozane/compare/v0.5.0...v0.6.0) (2026-09-09)


### Features

* an edition can cap how much of its digest one subject takes ([#79](https://github.com/yaad-index/roozane/issues/79)) ([40dc7e4](https://github.com/yaad-index/roozane/commit/40dc7e416a6dd1432b2a945c0c471e3ca93ca5e1))

## [0.5.0](https://github.com/yaad-index/roozane/compare/v0.4.1...v0.5.0) (2026-09-08)


### Features

* the digest is written in the edition's language ([#72](https://github.com/yaad-index/roozane/issues/72)) ([5a6b250](https://github.com/yaad-index/roozane/commit/5a6b250e6077f931ef55637c1c058b1f78c86f07))

## [0.4.1](https://github.com/yaad-index/roozane/compare/v0.4.0...v0.4.1) (2026-09-08)


### Bug Fixes

* a failed selection costs one item, not the whole edition ([#68](https://github.com/yaad-index/roozane/issues/68)) ([e3837f0](https://github.com/yaad-index/roozane/commit/e3837f06770167946a3a3067628bbc445bab8037))
* one event is one digest entry, not one entry per article ([#62](https://github.com/yaad-index/roozane/issues/62)) ([669e996](https://github.com/yaad-index/roozane/commit/669e9961624f505a54911a6bf6dd2beb67a2bf21))

## [0.4.0](https://github.com/yaad-index/roozane/compare/v0.3.0...v0.4.0) (2026-09-07)


### Features

* editions and the sink binding ([#47](https://github.com/yaad-index/roozane/issues/47)) ([a5c27ea](https://github.com/yaad-index/roozane/commit/a5c27eabe30ad0151145410d1c89da5d34f7e7d4))
* empty digests carry their sources' collection outcomes ([#53](https://github.com/yaad-index/roozane/issues/53)) ([14742f9](https://github.com/yaad-index/roozane/commit/14742f9210b1f7f796d7a018555ac4077f34b2ed))
* neutral enrichment pass, then per-edition selection ([#49](https://github.com/yaad-index/roozane/issues/49)) ([626daae](https://github.com/yaad-index/roozane/commit/626daaece5a9e88d6853eee6a81a80a3f14ee6e9))
* persist per-source collection outcomes as telemetry ([#51](https://github.com/yaad-index/roozane/issues/51)) ([f895fb6](https://github.com/yaad-index/roozane/commit/f895fb62b23325c3d4b9bd3be169ecfe5adebefb))
* the daily report, and a sink that can name it ([#54](https://github.com/yaad-index/roozane/issues/54)) ([c985316](https://github.com/yaad-index/roozane/commit/c98531604ac759a2f590546ddfb1d2590d22fc6f))

## [0.3.0](https://github.com/yaad-index/roozane/compare/v0.2.0...v0.3.0) (2026-09-05)


### Features

* exec collector runner and the ADR-0003 permission refusal ([#35](https://github.com/yaad-index/roozane/issues/35)) ([69106c6](https://github.com/yaad-index/roozane/commit/69106c6a89c32f40ba862a3b9bc61502a8d77a3d))
* publish a container image to GHCR on release ([#32](https://github.com/yaad-index/roozane/issues/32)) ([8e94582](https://github.com/yaad-index/roozane/commit/8e945821c07f5f0825b523d1388691bff13cc9f9))


### Bug Fixes

* bound plugin Wait so an escaped descendant cannot hang a run ([#37](https://github.com/yaad-index/roozane/issues/37)) ([1d91784](https://github.com/yaad-index/roozane/commit/1d917845e97ce88c8f64623720736ea8ed78ddd6))

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
