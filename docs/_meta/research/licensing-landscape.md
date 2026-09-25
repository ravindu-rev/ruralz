# Open-Source Licensing and Monetization Landscape

| Field | Value |
|---|---|
| Topic | Licensing, relicensing events, governance models, monetization patterns, trademark policies, contribution agreements and supply-chain expectations relevant to a fully Apache-2.0 API gateway |
| Snapshot date | 2026-09-23 |
| Method | Web search plus direct fetches of vendor blogs, docs, pricing pages, GitHub repositories/releases/discussions, foundation policy pages and specification sites. Each claim carries its source URL on the same line. Where only a secondary source was available, the text says so. Contradictions and unverified items are listed under Gaps. |

## 1. Relicensing and edition-split events (chronological)

| Date (ISO) | Project | Event | Source |
|---|---|---|---|
| 2021-04-21 | Grafana, Loki, Tempo | Relicensed from Apache-2.0 to AGPLv3; plugins, agents and some libraries stayed Apache-2.0 | (https://grafana.com/blog/grafana-loki-tempo-relicensing-to-agplv3/) |
| 2023-08-10 | HashiCorp (Terraform, Vault, Consul, Nomad, Packer, Vagrant, Boundary, Waypoint) | MPL-2.0 to Business Source License 1.1 for future releases | (https://www.hashicorp.com/blog/hashicorp-adopts-business-source-license) |
| 2024-01-10 | OpenTofu | 1.6 GA as a Linux Foundation project (fork of pre-BSL Terraform) | (https://www.linuxfoundation.org/press/opentofu-announces-general-availability) |
| 2024-03 | Redis | BSD-3-Clause to dual RSALv2/SSPLv1 (from Redis 7.4) | (https://redis.io/blog/agplv3/) (https://redis.io/legal/licenses/) |
| 2024-03-28 | Valkey | Linux Foundation announces Valkey fork of Redis 7.2.4 under BSD-3-Clause | (https://www.linuxfoundation.org/press/linux-foundation-launches-open-source-valkey-community) |
| 2024-08-29 | Elasticsearch, Kibana | AGPLv3 added as a third option alongside SSPL and ELv2 | (https://www.elastic.co/blog/elasticsearch-is-open-source-again) |
| 2025-02-27 | HashiCorp | IBM closes USD 6.4B acquisition | (https://techcrunch.com/2025/02/27/ibm-closes-6-4b-hashicorp-acquisition/) |
| 2025-04-23 | OpenTofu | Accepted into CNCF Sandbox | (https://www.cncf.io/projects/opentofu/) |
| 2025-05-01 | Redis 8.0 | AGPLv3 added as a third license option (RSALv2 / SSPLv1 / AGPLv3) | (https://redis.io/blog/agplv3/) |
| 2025-10-21 | Valkey 9.0 | GA | (https://www.linuxfoundation.org/press/valkey-9.0-delivers-performance-and-resiliency-for-real-time-workloads) |
| 2026-05-19 | Valkey 9.1 | Release with Valkey Search 1.2 | (https://www.linuxfoundation.org/press/valkey-enhances-efficiency-security-and-modular-performance-with-9.1-release-and-new-ecosystem-integrations) |

## 4. HashiCorp BSL and OpenTofu

- HashiCorp moved from MPL-2.0 to BSL 1.1 on 2023-08-10 for all future releases of HashiCorp products; APIs, SDKs and almost all other libraries stayed MPL-2.0 (https://www.hashicorp.com/blog/hashicorp-adopts-business-source-license).
- HashiCorp's Additional Use Grant permits production use "provided Your use does not include offering the Licensed Work to third parties on a hosted or embedded basis in order to compete with HashiCorp's paid version(s)"; Change Date is four years after publication; Change License is MPL-2.0 (https://www.hashicorp.com/en/bsl).
- OpenTofu 1.6 reached GA on 2024-01-10 under the Linux Foundation, with named supporters including Cloudflare, Buildkite, GitLab and Oracle (https://www.linuxfoundation.org/press/opentofu-announces-general-availability).
- OpenTofu was accepted into the CNCF at Sandbox level on 2025-04-23 (https://www.cncf.io/projects/opentofu/). Secondary reporting states the CNCF Governing Board granted an IP-policy exception to allow MPL-2.0 (CNCF normally requires Apache-2.0) (https://thenewstack.io/opentofu-joins-cncf-new-home-for-open-source-iac-project/).
- IBM closed its acquisition of HashiCorp on 2025-02-27 for USD 6.4B (https://techcrunch.com/2025/02/27/ibm-closes-6-4b-hashicorp-acquisition/).

## 5. Redis, Valkey and Elastic

| Redis version | License options | Source |
|---|---|---|
| 7.2 and earlier | BSD-3-Clause | (https://redis.io/legal/licenses/) |
| 7.4 | RSALv2 or SSPLv1 | (https://redis.io/legal/licenses/) |
| 8.0+ | RSALv2 or SSPLv1 or AGPLv3 | (https://redis.io/legal/licenses/) |

- The Redis 8 announcement (2025-05-01, Rowan Trollope) says AGPL was added "starting with Redis 8", Redis Stack modules (JSON, Time Series, probabilistic types, Query Engine) were folded into core Redis 8 under AGPL, and vector sets were introduced (https://redis.io/blog/agplv3/).
- The same post acknowledges the 2024 SSPL move "hurt our relationship with the Redis community" and that "AWS and Google now maintain their own fork" (https://redis.io/blog/agplv3/).
- Valkey was announced 2024-03-28 by the Linux Foundation, continuing from Redis 7.2.4 under BSD-3-Clause (https://www.linuxfoundation.org/press/linux-foundation-launches-open-source-valkey-community).
- Valkey 9.0 GA (2025-10-21) added atomic slot migration, hash field expiration and multiple databases in cluster mode (https://www.linuxfoundation.org/press/valkey-9.0-delivers-performance-and-resiliency-for-real-time-workloads).
- Valkey 9.1 (2026-05-19) reduced per-key memory usage by up to 10% for common workloads, added automated TLS certificate reload and database-level ACLs, and shipped alongside Valkey Search 1.2 (full-text, numeric, tag and vector search) (https://www.linuxfoundation.org/press/valkey-enhances-efficiency-security-and-modular-performance-with-9.1-release-and-new-ecosystem-integrations).
- Elastic relicensed Elasticsearch/Kibana to ELv2 + SSPL in January 2021 and added AGPLv3 as a third option on 2024-08-29; client libraries remain Apache-2.0 and binary distributions did not change (https://www.elastic.co/pricing/faq/licensing).

Relevance to the State Store (analysis): Ruralz talks to Redis/Valkey over the network via a client library (`rueidis`); it does not link or distribute the server, so the server's license choice (BSD / RSAL / SSPL / AGPL) does not propagate to Ruralz binaries. Valkey (BSD-3-Clause) is the backend with no copyleft or source-available terms at all.

## 8. Open-core vs cloud-first monetization

### 8.2 Cloud-first OSS reference companies

| Company | OSS license | Cloud tiers | Paid-only governance features | Source |
|---|---|---|---|---|
| Grafana Labs | AGPLv3 (Grafana, Loki, Tempo since 2021) | Free USD 0; Pro from USD 19/month + usage; Enterprise min USD 25,000/year | Grafana Enterprise: SAML, team sync, enhanced LDAP, data source permissions, auditing, reporting, fine-grained RBAC, Vault integration, 40+ enterprise plugins | (https://grafana.com/blog/grafana-loki-tempo-relicensing-to-agplv3/) (https://grafana.com/pricing/) (https://grafana.com/docs/grafana/latest/introduction/grafana-enterprise/) |
| Supabase | Apache-2.0 (main repo) | Free USD 0; Pro USD 25/month; Team USD 599/month; Enterprise custom | Platform audit logs and SOC2/ISO 27001 from Team; SSO (SAML 2.0) from Pro (50 MAU included, then USD 0.015/MAU); custom scoped roles on Enterprise | (https://github.com/supabase/supabase/blob/master/LICENSE) (https://supabase.com/pricing) |

Free tier limits for Grafana Cloud: 10k active metric series, 50 GB logs, 50 GB traces, 3 active users, 14-day retention (https://grafana.com/pricing/).

### 8.3 What typically stays paid in open-core products, and what Ruralz gives away

| Capability | Typically paid at | Ruralz position (from foundation pack) |
|---|---|---|
| SSO (SAML/OIDC) for admin UI | Grafana Enterprise (https://grafana.com/docs/grafana/latest/introduction/grafana-enterprise/); Supabase Pro and above (https://supabase.com/pricing) | Free; SSO/SAML for Console is `Planned (M5)` |
| RBAC (fine-grained / custom roles) | Grafana Enterprise fine-grained RBAC (https://grafana.com/docs/grafana/latest/introduction/grafana-enterprise/); Supabase Enterprise custom scoped roles (https://supabase.com/pricing) | Free; RBAC hosted by Ruralz Control |
| Audit log | Grafana Enterprise auditing (https://grafana.com/docs/grafana/latest/introduction/grafana-enterprise/); Supabase Team (https://supabase.com/pricing) | Free; audit log hosted by Ruralz Control |
| Managed hosting, SLA, 24/7 support | Grafana Cloud (https://grafana.com/pricing/); Supabase (https://supabase.com/pricing) | Paid: Ruralz Cloud and commercial support (the only revenue lines) |

## 9. Source-available alternatives (context)

- The Functional Source License (FSL), created by Sentry under the Fair Source initiative, converts each version to Apache-2.0 or MIT two years after release and forbids competing commercial use until then (https://fsl.software/).
- HashiCorp's BSL uses a four-year change date to MPL-2.0 (https://www.hashicorp.com/en/bsl).
- Redis RSALv2 prohibits commercializing or offering Redis as a managed service to third parties; SSPLv1 requires releasing the service's management layers when offered as a service; neither is OSI-approved (https://redis.io/legal/licenses/).

Pattern (analysis): three of the four relicensing companies above (Redis, Elastic, Grafana) settled on AGPLv3 as an OSI-approved defensive license, and HashiCorp chose the source-available BSL; none returned to a permissive license. Ruralz's choice of Apache-2.0 plus trademark control is the permissive alternative, relying on the Ruralz mark and managed-cloud value rather than copyleft.

## 10. Trademark policies as models for a Revington policy

| Policy | Key permissions | Key restrictions | Source |
|---|---|---|---|
| Apache Software Foundation | Nominative fair use ("Apache Foo is faster than X"); "Powered by" forms in specific situations; logos may link to apache.org | No ASF marks in product branding; derivatives may not use confusingly similar marks; no ASF product names as second-level domains; other logo use needs VP Brand Management approval | (https://www.apache.org/foundation/marks/) |
| Grafana Labs | Referencing marks for OSS discussion, development and support; adjective + generic noun | No marks in product, domain or social-handle names; no use with derivative or modified software; commercial use needs written permission; required non-affiliation statement | (https://grafana.com/trademark-policy/) |
| Redis (updated 2025-02-26) | Docs reference with attribution; "integrates version XX of Redis"; OSS projects that disclaim endorsement | No "Redis" or "Red" in product/company/domain names; a modified fork may not be called "Redis compatible" (use "includes code based on a fork of Redis OSS version XX"); exposing the Redis API as a hosted service requires a commercial agreement to use the marks | (https://redis.io/legal/trademark-policy/) |
| Linux Foundation | Fair use for truthful factual statements; "<product> compatible with <mark>" or "<product> for <mark>" | Marks as adjectives only; not in product or domain names; no certification claims without authorization | (https://www.linuxfoundation.org/legal/trademark-usage) |
| LF Projects (hosted projects) | "compatible with" / "for use with" if accurate | "a copyright license, even an open source copyright license, does not include an implied right or license to use a trademark"; no marks as product or domain names | (https://lfprojects.org/policies/trademark-policy/) |

- Apache-2.0 Section 6 itself states the license "does not grant permission to use the trade names, trademarks, service marks, or product names of the Licensor, except as required for reasonable and customary use in describing the origin of the Work and reproducing the content of the NOTICE file" (https://www.apache.org/licenses/LICENSE-2.0).

Elements a Revington policy can borrow (analysis): ASF/LF nominative use and "X for Ruralz" / "compatible with Ruralz" phrasing; Grafana's rule that modified builds may not carry the mark; Redis's rule that a hosted service reselling the product under its name needs an agreement (protecting "Ruralz Cloud" without restricting the code); the LF Projects statement separating copyright and trademark rights.

## 11. DCO vs CLA

| Dimension | DCO 1.1 | CLA (for example CNCF/EasyCLA) | Source |
|---|---|---|---|
| Mechanism | `Signed-off-by:` line per commit certifying clauses (a)-(d) | Signed individual and/or corporate agreement before first contribution | (https://developercertificate.org/) (https://helm.sh/blog/helm-dco/) (https://docs.linuxfoundation.org/lfx/easycla/v2-current/getting-started/easycla-faqs) |
| Contributor friction | Low; no employer paperwork | Corporate CLA signature can take weeks | (https://helm.sh/blog/helm-dco/) |
| Patent grant | Relies on the project license (Apache-2.0 Section 3) | Explicit in the CLA | (https://helm.sh/blog/helm-dco/) |
| Relicensing ability for the company | With Apache-2.0 inbound = outbound, contributions arrive under Apache-2.0 (Section 5), which already lets any redistributor, Revington included, sublicense and ship Derivative Works as a whole under additional or different terms (Sections 2 and 4); the contributed code itself stays available under Apache-2.0 | Typically broad (assignment or a broad license grant to the company) | (https://www.apache.org/licenses/LICENSE-2.0) |
| CNCF practice | Most CNCF projects used DCO (as of 2018) | Exceptions: Kubernetes and gRPC | (https://helm.sh/blog/helm-dco/) |
| Project examples | Helm moved CLA to DCO on 2018-08-27 | Kubernetes and gRPC kept CLAs as CNCF exceptions | (https://helm.sh/blog/helm-dco/) |

- DCO clause (d) records that contributions and the contributor's personal information are public and "maintained indefinitely" (https://developercertificate.org/).
- The CNCF considered the patent grant and warranty disclaimers of a CLA already covered by Apache-2.0 when Helm moved to DCO (https://helm.sh/blog/helm-dco/).
- Apache-2.0 Section 5: "any Contribution intentionally submitted for inclusion in the Work by You to the Licensor shall be under the terms and conditions of this License, without any additional terms or conditions" (https://www.apache.org/licenses/LICENSE-2.0).
- Apache-2.0 Section 2 grants each recipient "a perpetual, worldwide, non-exclusive, no-charge, royalty-free, irrevocable copyright license to reproduce, prepare Derivative Works of, publicly display, publicly perform, sublicense, and distribute the Work and such Derivative Works" (https://www.apache.org/licenses/LICENSE-2.0).
- Apache-2.0 Section 4, final paragraph: "You may add Your own copyright statement to Your modifications and may provide additional or different license terms and conditions for use, reproduction, or distribution of Your modifications, or for any such Derivative Works as a whole, provided Your use, reproduction, and distribution of the Work otherwise complies with the conditions stated in this License" (https://www.apache.org/licenses/LICENSE-2.0).
- The conditions that still apply to such a redistribution are Section 4(a)-(d): give recipients a copy of the License, mark modified files, retain copyright, patent, trademark and attribution notices, and carry the NOTICE attributions (https://www.apache.org/licenses/LICENSE-2.0).

Tradeoff for Ruralz (analysis, correcting an earlier revision of this section): a DCO does **not** stop Revington from shipping future Ruralz releases under different terms. Because contributions arrive under Apache-2.0 (Section 5), and Apache-2.0 lets any redistributor provide "additional or different license terms" for its modifications or for a Derivative Work as a whole while complying with Section 4(a)-(d) (https://www.apache.org/licenses/LICENSE-2.0), Revington, like any other redistributor, could distribute a later Derivative Work that includes community contributions under proprietary or source-available terms without asking contributors. What a DCO with Apache-2.0 does guarantee is narrower: each contribution, and every release already published under Apache-2.0, stays licensed under Apache-2.0 to everyone under a perpetual, irrevocable grant (Section 2), so the community can always fork the last Apache-2.0 release. A CLA adds rights on top of that, for example relicensing the contributed code itself as if Revington owned it; the DCO forgoes that, but it is not a legal barrier to a HashiCorp/Redis-style change of terms for new releases. If the "fully free" positioning needs a binding commitment, it has to come from something other than the DCO, such as a published license pledge, foundation stewardship or trademark policy; this needs legal review before ADR-0002 relies on it.

## 12. Apache-2.0 patent clause implications

- Section 3 grants each contributor's patent license, limited to "those patent claims licensable by such Contributor that are necessarily infringed by their Contribution(s) alone or by combination of their Contribution(s) with the Work", and terminates it for anyone who institutes patent litigation (including a cross-claim or counterclaim) alleging the Work infringes, "as of the date such litigation is filed" (https://www.apache.org/licenses/LICENSE-2.0).
- The FSF considers Apache-2.0 compatible with GPLv3 (one direction only: Apache-2.0 code can go into GPLv3 works) and has never considered it compatible with GPLv2, citing the patent termination and indemnification provisions (https://www.apache.org/licenses/GPL-compatibility.html).
- Practical consequences for Ruralz (analysis): (1) Revington's own contributions carry a patent grant, which enterprise adopters usually ask about; (2) the grant covers only claims read on by contributed code, not unrelated Revington patents; (3) a Plugin compiled to WASM that links GPLv2-only code cannot be distributed together with Ruralz as one work, though separately distributed Plugins loaded at runtime are a separate question needing counsel; (4) AGPLv3 components (Grafana, Redis 8 AGPL option) are not linked into Ruralz and communicate over the network.

## 13. Supply-chain expectations in 2026

| Area | Current state (2026-09-23) | Source |
|---|---|---|
| SLSA | v1.2 approved and announced 2025-11-24; adds a Source Track (L2 history and provenance, L3 continuous enforcement of technical controls); backwards compatible with v1.1; v1.1 marked retired | (https://slsa.dev/blog/2025/11/announce-slsa-v1.2) (https://slsa.dev/spec/v1.2/whats-new) (https://slsa.dev/spec/v1.1/levels) |
| SLSA Build levels | L1 provenance exists; L2 signed provenance from a hosted build platform; L3 hardened build platform isolating runs and protecting signing material | (https://slsa.dev/spec/v1.1/levels) |
| GitHub artifact attestations | SLSA Build L2 by default; L3 with reusable workflows; public repos use the Sigstore Public Good instance; SBOM attestations supported | (https://docs.github.com/en/actions/concepts/security/artifact-attestations) |
| cosign | v3.1.3 and v2.6.5 released 2026-08-06 (fix for GHSA-fx35-mq7g-6g98, verification bypass via public key in legacy bundle); v3.1.1 (2026-06-09) made the bundle format the default and moved signing to Rekor v2 with DSSE hashed entries | (https://github.com/sigstore/cosign/releases) |
| Rekor | Rekor v2 GA 2025-10-10: tile-backed log, yearly shards (for example `log2025-1.rekor.sigstore.dev`), single `/api/v2/log/entries` endpoint | (https://blog.sigstore.dev/rekor-v2-ga/) |
| SBOM: CycloneDX | v1.7 released October 2025; ECMA-424 2nd edition (December 2025) standardizes v1.7; v1.6 was ECMA-424 1st edition (June 2024) | (https://cyclonedx.org/news/cyclonedx-v1.7-released/) (https://ecma-international.org/publications-and-standards/standards/ecma-424/) |
| SBOM: SPDX | 3.0.1 current; SPDX 2.2.1 was prepared for ISO submission and ISO/IEC 5962 became available 2021-08 | (https://spdx.github.io/spdx-spec/latest/) (https://spdx.dev/about/overview/) |
| OpenSSF Scorecard | Latest release listed v5.5.0 (2026-04-23); checks include Binary-Artifacts, Branch-Protection, Code-Review, Dangerous-Workflow, Dependency-Update-Tool, Fuzzing, Pinned-Dependencies, SAST, Security-Policy, Signed-Releases, Token-Permissions, Vulnerabilities; weights Critical 10, High 7.5, Medium 5, Low 2.5 | (https://github.com/ossf/scorecard/releases) (https://github.com/ossf/scorecard) |
| OpenSSF OSPS Baseline | Initial release 2025-02-25; three maturity levels across categories including Access Control, Build and Release, Documentation, Governance, Legal, Quality, Security Assessment and Vulnerability Management; current version v2026.08.28 | (https://openssf.org/press-release/2025/02/25/openssf-announces-initial-release-of-the-open-source-project-security-baseline/) (https://baseline.openssf.org/) (https://baseline.openssf.org/versions/2026-08-28) |
| EU Cyber Resilience Act | Manufacturer reporting of actively exploited vulnerabilities and severe incidents via ENISA's Single Reporting Platform from 2026-09-11 (24h early warning, 72h notification); open-source stewards have a lighter regime, with obligations cited from 2027-12-11 | (https://digital-strategy.ec.europa.eu/en/policies/cra-reporting) (https://www.goodwinlaw.com/en/insights/publications/2026/09/alerts-lifesciences-technology-preparing-for-eu-cyber-resilience-act) |

Baseline expectation for a Go gateway shipping images to GHCR in 2026 (analysis based on the rows above): keyless cosign signatures in bundle format on `ghcr.io/ravindu-rev/ruralzd` and `ghcr.io/ravindu-rev/ruralz-control`; SLSA Build L3 provenance via reusable workflows; CycloneDX 1.6/1.7 and/or SPDX SBOMs attached as attestations; a published Scorecard; and a vulnerability-reporting process that fits CRA timelines. Revington would likely count as a manufacturer for Ruralz Cloud and possibly as an open-source steward for the Apache-2.0 project; that classification needs legal review.

## Sources

https://www.hashicorp.com/blog/hashicorp-adopts-business-source-license
https://www.hashicorp.com/en/bsl
https://www.linuxfoundation.org/press/opentofu-announces-general-availability
https://www.cncf.io/projects/opentofu/
https://thenewstack.io/opentofu-joins-cncf-new-home-for-open-source-iac-project/
https://techcrunch.com/2025/02/27/ibm-closes-6-4b-hashicorp-acquisition/
https://redis.io/blog/agplv3/
https://redis.io/legal/licenses/
https://redis.io/legal/trademark-policy/
https://www.linuxfoundation.org/press/linux-foundation-launches-open-source-valkey-community
https://www.linuxfoundation.org/press/valkey-9.0-delivers-performance-and-resiliency-for-real-time-workloads
https://www.linuxfoundation.org/press/valkey-enhances-efficiency-security-and-modular-performance-with-9.1-release-and-new-ecosystem-integrations
https://www.elastic.co/blog/elasticsearch-is-open-source-again
https://www.elastic.co/pricing/faq/licensing
https://grafana.com/blog/grafana-loki-tempo-relicensing-to-agplv3/
https://grafana.com/pricing/
https://grafana.com/docs/grafana/latest/introduction/grafana-enterprise/
https://grafana.com/trademark-policy/
https://github.com/supabase/supabase/blob/master/LICENSE
https://supabase.com/pricing
https://fsl.software/
https://www.apache.org/foundation/marks/
https://www.linuxfoundation.org/legal/trademark-usage
https://lfprojects.org/policies/trademark-policy/
https://www.apache.org/licenses/LICENSE-2.0
https://www.apache.org/licenses/GPL-compatibility.html
https://developercertificate.org/
https://helm.sh/blog/helm-dco/
https://docs.linuxfoundation.org/lfx/easycla/v2-current/getting-started/easycla-faqs
https://slsa.dev/spec/v1.1/levels
https://slsa.dev/spec/v1.2/whats-new
https://slsa.dev/blog/2025/11/announce-slsa-v1.2
https://docs.github.com/en/actions/concepts/security/artifact-attestations
https://github.com/sigstore/cosign/releases
https://blog.sigstore.dev/rekor-v2-ga/
https://cyclonedx.org/news/cyclonedx-v1.7-released/
https://ecma-international.org/publications-and-standards/standards/ecma-424/
https://spdx.github.io/spdx-spec/latest/
https://spdx.dev/about/overview/
https://github.com/ossf/scorecard
https://github.com/ossf/scorecard/releases
https://openssf.org/press-release/2025/02/25/openssf-announces-initial-release-of-the-open-source-project-security-baseline/
https://baseline.openssf.org/
https://baseline.openssf.org/versions/2026-08-28
https://digital-strategy.ec.europa.eu/en/policies/cra-reporting
https://www.goodwinlaw.com/en/insights/publications/2026/09/alerts-lifesciences-technology-preparing-for-eu-cyber-resilience-act

## Gaps

- **Grafana Cloud** pricing page does not map SSO/RBAC/audit to Cloud tiers; the Enterprise-only list is from Grafana Enterprise (self-managed) docs.
- **OpenSSF Scorecard:** the releases page lists v5.5.0 (2026-04-23) as the latest. The README table lists 19 default checks.
- **OSPS Baseline:** individual control IDs for signed releases/SBOM/provenance at each level were not extracted from v2026.08.28.
- **CRA:** the Commission page confirms the 2026-09-11 reporting start for manufacturers and steward reporting obligations (Article 24(3)) from 2027-12-11; the steward cybersecurity-policy obligations come from the law-firm source. Whether Revington counts as a manufacturer or steward for the Apache-2.0 project was not determined.
- **OpenTofu CNCF IP exception (MPL-2.0)** comes from secondary reporting; the CNCF project page does not state the license.
- **Redis trademark policy on "Red":** verified in the primary text: "Don't put our Mark (or part of our name, e.g., "Red") in your company name, commercial product name, domain name, or social media handle".
- **DCO and relicensing (corrected 2026-09-23):** an earlier revision of section 11 said a DCO prevents Revington from unilaterally relicensing contributions. The Apache-2.0 text (Sections 2, 4 and 5; https://www.apache.org/licenses/LICENSE-2.0) does not support that for Apache-2.0 inbound contributions, and section 11 now says so. How far "additional or different license terms ... for any such Derivative Works as a whole" reaches in practice (for example, whether a recipient of a proprietary build may still use the Apache-2.0 portions under Apache-2.0) is a legal-interpretation question; no court decision or counsel opinion was consulted.
- **Apache-2.0 and WASM Plugins linking GPLv2 code:** stated as a question for legal counsel; no authoritative source on WASM module linkage was found.

