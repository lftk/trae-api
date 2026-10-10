# Protocol references

This Go implementation was written against the following local source revisions:

- [autumnsentiment/Trae2api-cn](https://github.com/autumnsentiment/Trae2api-cn), commit `0075bb663c22ec51a443f614764e079a27277248`: `web_login.py` and `src/auth.py` for the legacy browser authorization / ExchangeToken flow; `src/trae_client.py` and `src/raw_client.py` for CN IDE headers, `solo_work_lite`, native content blocks and string-encoded tool schemas; `src/sse.py`, `src/responses_api.py`, and native-tool regression tests for SSE framing and Responses adaptation.
- [wangqi233/trae2api](https://github.com/wangqi233/trae2api), commit `3da7b58de0303136479303a4f773c31f72306a29`: `src/realms.js`, `src/auth.js` and `src/trae-client.js` for separation of the CN authentication host and model gateway, credential refresh and region-specific behavior.

The gateway implements only the stateless Chat Completions, single-account CN IDE path with in-memory credentials and official browser login. It does not implement Responses, persistence, account rotation, client tool execution or management dashboards. Offline fixtures model the documented native event shapes; successful offline tests do not establish live upstream or real-login availability. No real-account test is included in this scope.

The native protocol is undocumented and may change. The model host, authorization host, authorization URL, client ID and version headers are fixed internal protocol defaults, not user configuration. The default SOLO browser flow deliberately omits `code_challenge` and accepts `userJwt` or a refresh token, matching the referenced local authorization helper. An authorization-code-only callback is rejected instead of guessing an exchange protocol.

Both reference projects carry MIT licenses. Attribution notices are retained below for the protocol adaptations.

## Trae2api-cn

MIT License

Copyright (c) 2026 autumnsentiment

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## trae2api

MIT License

Copyright (c) 2026 trae2api contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
