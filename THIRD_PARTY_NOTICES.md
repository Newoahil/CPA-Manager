# Third-party notices

CPA Manager source: <https://github.com/Newoahil/CPA-Manager>.
Project license: **LGPL-3.0-only**, with the complete GNU texts in
[`COPYING.LESSER`](COPYING.LESSER) and [`COPYING`](COPYING). Third-party material
retains its own copyrights and applicable license terms. No author, project,
or service mentioned here endorses CPA Manager.

## Ollama settings-page adaptation

`internal/ollama/client.go` and `internal/ollama/parse.go` are conservatively
distributed as an LGPL version 3 adaptation. The pre-publication source already
credited the following provenance chain; this release preserves that attribution
rather than claiming a clean-room implementation:

1. [jacklee-code/ollama-cloud-quota-monitor](https://github.com/jacklee-code/ollama-cloud-quota-monitor),
   [`usage.go`](https://github.com/jacklee-code/ollama-cloud-quota-monitor/blob/a3a95881f8977e766441adcb4980d08c705c226d/usage.go),
   comparison commit `a3a95881f8977e766441adcb4980d08c705c226d`.
   Its README credits the parser to Wei-Shaw/sub2api and declares LGPL-3.0;
   its [LICENSE](https://github.com/jacklee-code/ollama-cloud-quota-monitor/blob/a3a95881f8977e766441adcb4980d08c705c226d/LICENSE)
   contains the GNU LGPL version 3 text.
2. [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api),
   [`backend/internal/service/ollama_cloud_usage_parser.go`](https://github.com/Wei-Shaw/sub2api/blob/a60a29549f488a854966aaec9541abbe006cac22/backend/internal/service/ollama_cloud_usage_parser.go),
   comparison commit `a60a29549f488a854966aaec9541abbe006cac22`.
   Its [LICENSE](https://github.com/Wei-Shaw/sub2api/blob/a60a29549f488a854966aaec9541abbe006cac22/LICENSE)
   contains the GNU LGPL version 3 text.

These commits are **release-time comparison anchors reviewed on 2026-09-29**,
not a claim that those exact commits were used during development. The original
development revisions and the exact extent of adaptation were not recorded.

Local modifications/differences documented on 2026-09-29:
- CPA Manager represents results as its own credential/quota snapshots with
  scrubbed authentication/transport/parse failures.
- The local parser flattens DOM block elements into lines and searches nearby
  lines for 5h/7d percentages and reset text. The compared upstream parser
  searches label elements and ancestor blocks, including attribute/track data.
- The local implementation has its own label/plan/balance heuristics, relative
  duration parsing, and numeric-window success condition; it does not implement
  the compared plugin's account management, model request counts or key syncing.
- The release packaging changes attribution/license documentation and the Go
  module path; they do not alter these parsing algorithms.

At those comparison anchors, neither cited parser source file contains a
separate author copyright header. Their LICENSE files contain
`Copyright (C) 2007 Free Software Foundation, Inc. <https://fsf.org/>`, which is
the copyright notice for the **license document**, not an attribution of parser
authorship to the FSF. That notice is preserved in COPYING.LESSER. Upstream
project attribution is preserved above; no unverified copyright holder or year
has been invented for upstream code. Local modifications are copyright (C) 2026
CPA Manager contributors. Consult the linked upstream source/history for its
contributors; the project's LGPL-3.0-only choice does not remove any permissions
an upstream may independently grant.

## Go module dependencies

The versions below match go.mod/go.sum. Complete upstream license texts,
including original copyright notices and disclaimers, accompany this repository
and are copied into the runtime image; the links are provenance, not substitutes
for those texts.

| Module/version | License | Preserved copyright notice | Local full text / upstream source |
| --- | --- | --- | --- |
| `github.com/larksuite/oapi-sdk-go/v3 v3.12.0` | MIT | Copyright (c) 2020 Lark Technologies Pte. Ltd. | [local](licenses/third-party/larksuite-oapi-sdk-go-LICENSE) / [upstream](https://github.com/larksuite/oapi-sdk-go/blob/v3.12.0/LICENSE) |
| `golang.org/x/net v0.33.0` | BSD-3-Clause | Copyright 2009 The Go Authors. | [local](licenses/third-party/golang-x-net-LICENSE) / [upstream](https://github.com/golang/net/blob/v0.33.0/LICENSE) |
| `github.com/gogo/protobuf v1.3.2` | BSD-3-Clause | Copyright (c) 2013, The GoGo Authors. All rights reserved. Also: Copyright 2010 The Go Authors. All rights reserved. | [local](licenses/third-party/gogo-protobuf-LICENSE) / [upstream](https://github.com/gogo/protobuf/blob/v1.3.2/LICENSE) |
| `github.com/gorilla/websocket v1.5.0` | BSD-2-Clause | Copyright (c) 2013 The Gorilla WebSocket Authors. All rights reserved. | [local](licenses/third-party/gorilla-websocket-LICENSE) / [upstream](https://github.com/gorilla/websocket/blob/v1.5.0/LICENSE) |

The LGPL project declaration does not replace these dependency licenses.
Retain these texts when distributing the linked executable. If the linked
dependency set changes, update this inventory against the actual build.

## Go toolchain and container components

The builder is `golang:1.24`; the final stage copies the compiled executable,
not the builder filesystem. The executable includes Go runtime/standard-library
code. Dockerfile copies the actual builder's `/usr/local/go/LICENSE` to
`/usr/share/licenses/cpa-manager/third-party/go-LICENSE` rather than assuming
that a floating toolchain tag always carries the same notice. For standalone
binary distribution, also include the LICENSE from the toolchain used to build
it. Go's license is BSD-3-Clause, copyright 2009 The Go Authors.

The runtime base is `gcr.io/distroless/static-debian12:nonroot`, from
[GoogleContainerTools/distroless](https://github.com/GoogleContainerTools/distroless).
Distroless build definitions are Apache-2.0, but this does **not** relicense all
Debian packages/data inside its images as Apache-2.0. Preserve the base image's
own component copyright/license files (including `/usr/share/doc` and
`/usr/share/common-licenses` where present). This Dockerfile does not delete them.
Because the base tag is mutable, a binary/image release must record the actual
digest and check its package/copyright inventory and any corresponding-source
requirements for that exact artifact; a repository URL is not that inventory.

Release-time inspection (2026-09-29) of the Linux/amd64 base selected by manifest
digest `sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab`
found full component copyright files under `/usr/share/doc` for `base-files`,
`ca-certificates`, `media-types`, `netbase`, and `tzdata`, plus the referenced
common license texts under `/usr/share/common-licenses`. The package notices
include GPL-2/GPL-2+, MPL-2.0, ad-hoc and public-domain terms; these describe
package material, not a blanket license for the whole container. Preserve those
notices and inspect the files actually redistributed when publishing an image.
This observed digest is an audit anchor, not a pin in the Dockerfile.

The final image additionally contains this notice, LICENSE, COPYING,
COPYING.LESSER and the module license texts under
`/usr/share/licenses/cpa-manager/`. The OCI source label points to the repository;
the revision label is supplied with `--build-arg REVISION=<source-commit>`.
Neither label substitutes for making the corresponding source available.

## Source and rebuilding

Keep the complete project source, tests, go.mod/go.sum, Dockerfile and Compose
file together with these notices. README documents rebuilding after modifying
the LGPL-covered portion. For a distributed static executable/image, provide
recipients equivalent access to the matching source and necessary build
materials/dependency sources, under terms allowing modification and rebuilding;
do not rely on a shared-library replacement claim. Identify the exact source
commit next to the artifact download and retain its source availability.

This is a source/provenance and release-packaging record, not a representation
that every future binary image or distribution has undergone a legal audit.
