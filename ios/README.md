# PAM iOS

Native iOS 17+ SwiftUI client. Open `PAM.xcodeproj` in Xcode 15 or later and select the shared `PAM` scheme. It supports iPhone and iPad. No third-party application dependencies are required.

The app provides transaction browsing, merchant/description search, direction and inclusive calendar-day filters, transaction details with original source data, paginated assets, and asset creation, editing, and deletion. The P0 Import tab adds experimental local screenshot parsing and confirmed REST imports; it never connects directly to PostgreSQL.

In Settings, enter the HTTPS backend origin (or a path prefix) and the REST access token. The URL is stored in preferences. The token is stored in Keychain with `WhenUnlockedThisDeviceOnly` accessibility; the existing token is never filled back into the input. Empty token input preserves the saved token. “Remove saved token” deletes it. The client refuses plain HTTP, URL credentials, query strings, and fragments, and refuses redirects to another origin. Use a trusted TLS certificate reachable from the device; no App Transport Security exceptions are installed.

Amounts remain decimal strings on the wire and in the UI. Asset prices accept up to 16 integer and 4 fractional digits, matching the backend. No financial calculation passes through `Double`. Empty purchase date, price, and related transaction remain unknown. A payment record alone is not treated as proof of the purchased product.

Transaction timestamps honor original `pam_timestamp_precision` metadata: date-only statements do not display an invented midnight, minute precision does not invent seconds, and second precision remains visible. Date-only records use `pam_source_date`, or the supplied statement's original `日期` field, to retain the source calendar day across device timezones.

The source contains a portable `PAMCore` Swift Package for REST model, URL/authentication, precise-decimal, date filtering, and HTTP error tests. From this directory, `swift test` runs these tests without SwiftUI or Keychain. These tests do not establish iOS UI or Keychain behavior.

On a Mac, `xcodebuild -project PAM.xcodeproj -scheme PAM -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO build` builds the app without a signing account. Run the scheme’s tests using an available simulator destination. A physical-device build requires an Apple development team, the normal device trust/signing setup, and an accessible HTTPS backend. Identity and device steps require separate validation.

Device acceptance should cover: save an HTTPS endpoint and valid token; browse and filter imported transactions; inspect source provenance; add/edit/delete an asset with a related transaction; refresh and paginate; retry after losing network access; restart the app and verify Keychain authentication persists. A Foundation test pass must not be reported as simulator/device acceptance.

## P0 statement import (source implementation; Apple build pending)

The Import tab selects multiple images through PhotosPicker and runs local Apple Vision. CMB, Alipay and WeChat have separate parser entry points over a shared coordinate/evidence model. Amounts remain decimal strings. OCR stores original text, normalized coordinates, confidence, image dimensions and SHA-256. Tall images use overlapping tiles sized relative to their actual width; tile ownership is determined by text center. Non-upright EXIF images are explicitly rejected.

The current CMB list layout inherits day groups and uses a user-confirmed capture date for Today/Yesterday. Alipay reads month/day plus time beneath the merchant; WeChat reads Chinese month/day plus time and a year from the header. These are experimental layout rules, not a guarantee for every historical version. Unsigned amounts, clipped/truncated text, missing fields, low confidence and balance discontinuity remain visible. Unknown layouts return a document-level diagnostic. Every layout currently requires explicit human review.

Original bytes are stored once per image SHA in the app's protected Application Support directory. Editable drafts and proposal IDs survive app restart. Per-row image crops are evidence previews; the original file is unchanged. REST receives only normalized fields and relevant OCR text/coordinates, not screenshot bytes. Backend preview and confirmation are separate actions. Pending reviews and source/account record ranges use the same backend as the web UI and MCP. Review editing is currently a complete-record JSON editor on iOS; a native field editor would improve usability.

`swift test` includes synthetic layout/money/date/boundary checks and a real-corpus test that explicitly skips without Apple Vision. The read-only `cmd/pam-statement-corpus` tool prepares private manifests from selected historical CSVs and images without modifying either. On an Apple host, `PAM_REAL_CORPUS`, `PAM_REAL_IMAGE_ROOT` and `PAM_REAL_REPORT_DIR` select the manifest, read-only image root and private report directory. The real test verifies image hashes and exact transaction multiset fields; it saves OCR/parse results privately. Capture dates can be supplied only when independently known. Historical CSV baselines are not automatically treated as newly audited ground truth.

The Linux NAS itself has no Xcode. GitHub-hosted macOS has now passed Swift tests, actual Vision recognition of a synthetic tall image, and a complete unsigned iOS Simulator build with Xcode 16.4. Simulator UI operation, signing, photo-picker interaction, real statement OCR and physical-device acceptance remain unverified. See `docs/p0-acceptance.md` for current results and sample gaps.

## GitHub Actions

This repository validates ready-for-review PRs with `.github/workflows/pam-ci.yml`. The Apple job uses a GitHub-hosted macOS runner for Swift tests, an actual Vision call on a synthetic tall PNG, and an unsigned iOS simulator build. The backend job initializes a disposable PostgreSQL 17 database with the normal separated roles, then runs tests, vet, race and independent HTTP/MCP acceptance. Draft PRs skip all jobs; there are no push or scheduled triggers.

Real statement screenshots and corpus manifests are not uploaded by this workflow. The real-corpus test continues to report an explicit skip unless separately authorized private input is supplied. Cloud compilation and synthetic OCR do not establish real-statement accuracy or physical-device acceptance.
