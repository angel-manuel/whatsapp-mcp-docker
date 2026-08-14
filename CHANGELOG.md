# Changelog

## [0.7.0](https://github.com/angel-manuel/whatsapp-mcp-docker/compare/v0.6.0...v0.7.0) (2026-08-14)


### Features

* **tools:** account status, presence, disappearing timers and read receipts ([7ba7433](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/7ba7433356530d228ca758bd6dd88dd73bad559b))
* **tools:** add account status, presence, timer and read-receipt tools ([a7a5ee3](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/a7a5ee31b604b6b77cf5b3e724fe0b4d59256ba0))
* **tools:** add send_file and send_audio_message media sends ([77d77f5](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/77d77f5e4657d445b6d2b1fd90fb38548a06f6a0))
* **tools:** add send_file and send_audio_message media sends ([252cfd7](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/252cfd79894cca46be9887c38156ec0395299ba5))
* **tools:** add send_poll, vote_poll and get_poll_results ([e501088](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/e5010882b6aef151c335cd1dbf09488c4bd86901))
* **tools:** add send_poll, vote_poll and get_poll_results ([eb889cd](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/eb889cd7a7c1c9ce3aeeb0c98438b2ccb42fe5bb))
* **tools:** add send_reaction and ingest incoming reactions ([62e1dc5](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/62e1dc5b8c40e5f17d08fcff5ef2b5b0f6da5d25))
* **tools:** support WhatsApp message reactions ([f341e7e](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/f341e7eeae17ea77825c4d156a78ab6a34f9f4d9))


### Bug Fixes

* **polls:** close the gaps vet found in the poll surface ([f4ead0a](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/f4ead0aaf9694d01d07adcfd6172e6cb75da086e))
* **reactions:** address vet review findings ([0f83a69](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/0f83a69a6824b0947f5eb5ce8fec21ef9b2c7c39))
* **tools:** address vet review on the account/presence tools ([acbce34](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/acbce3403a4f2e77cb346f835537876835597fa1))


### Refactoring

* **media:** address vet review of the media send path ([a2fdeb4](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/a2fdeb4c6cfe9a13949e00299f958396840637e4))


### Documentation

* reconcile SUPPORTED/README/DOCKERHUB/REQUIREMENTS with the code ([cc3dc75](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/cc3dc756e09ef46b3a1a9ee15a10e5fea589faef))
* reconcile SUPPORTED/README/DOCKERHUB/REQUIREMENTS with the code ([bc61ecc](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/bc61ecc0149ae427348062ed9dbcc21f9e20cd13))

## [0.6.0](https://github.com/angel-manuel/whatsapp-mcp-docker/compare/v0.5.1...v0.6.0) (2026-08-11)


### Features

* **tools:** add resolve_jid and follow jid_aliases in get_contact_details ([58fbfac](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/58fbfac7b398b5548a8506ec4ec406088f09654a))
* **tools:** add resolve_jid and follow jid_aliases in get_contact_details ([5485071](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/54850710600ea53caf80315ec992014268f0d698))

## [0.5.1](https://github.com/angel-manuel/whatsapp-mcp-docker/compare/v0.5.0...v0.5.1) (2026-08-05)


### Bug Fixes

* **ci:** point release-please at the existing vX.Y.Z tag format ([87d6bca](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/87d6bca4365e6ed92d01d9e6bee521dccf4b4832))
* **ci:** point release-please at the existing vX.Y.Z tag format ([f4873d1](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/f4873d11ae4d639d2183081c6829b03d1389713f))
* **server:** return promptly when the MCP transport exits on its own ([4bea80b](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/4bea80b818b0926c1d63c5c85004a91629aaa518))
* **server:** return promptly when the MCP transport exits on its own ([3cd5348](https://github.com/angel-manuel/whatsapp-mcp-docker/commit/3cd53482c9fa818670e12f093320c8ecaacdf902))
