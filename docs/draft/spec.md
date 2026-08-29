# ヘルスコネクト定期エクスポート 設計

## 背景・目的

ヘルスコネクトのデータ（血圧・心拍・睡眠など）を定期的に Google Drive 上へ書き出し、Claude から読めるようにする。

用途は3つ。**傾向の振り返り**、**項目間の相関分析**、**生データを都度自由に分析**。3つ目があるため、集約値だけでは足りない。

## 全体構成

```
Android ヘルスコネクト（定期エクスポート／毎日）
  ↓ Health Connect.zip（未暗号化 SQLite）
Google Drive「バックアップ」フォルダ
  ↓ ① 1時間おきにポーリング。modifiedTime が前回処理分より新しいときだけ実行
自宅Linuxサーバー（Docker コンテナ）
  ├ ② ZIP 展開 → エクスポート DB（一時ファイル）
  ├ ③ 累積 SQLite へ UPSERT（キー = レコード UUID）  ← 正史
  └ ④ 累積 DB から出力を生成
  ↓ ⑤ Sheets API で書き込み（サービスアカウント）
Google スプレッドシート1個（タブ複数）
  ↓
Claude（Drive コネクタ）
```

## 検討して見送った案

| 案 | 見送り理由 |
|---|---|
| GAS + Tasker/MacroDroid | Android 自動化ツールでの HC 読み出し。他の方向性を優先し保留 |
| Wellness Project | データが外部サービスのクラウドに乗る |
| Health Connect MCP（セルフホスト） | 外部サーバーへの設置を避けたい |

## 決定事項

### 認証：サービスアカウント

| 方式 | 評価 |
|---|---|
| rclone（OAuth） | 導入は手軽。ただし GCP のテストモードではリフレッシュトークンが7日で失効し、初回のブラウザ認証も必要。完全な非対話化ができない |
| **サービスアカウント（採用）** | JSON 鍵を `gcloud` で非対話的に発行できる。有効期限の管理が不要。Ansible での宣言的な運用と噛み合う |

**制約**：サービスアカウントは自身の保存容量を持たないため、**新規ファイルの作成（`create`）が容量エラーになる**。既存ファイルの読み取りと内容の書き換えは問題ない。

### 出力先：スプレッドシート1個＋タブ

上記の制約は「新規ファイル」にのみ効き、既存ファイル内部の構造変更には効かない。**空のスプレッドシートを1個だけ手で作って共有しておけば、以降のタブ追加・更新はサービスアカウントが全自動で行える。**

却下：種別ごとの CSV ファイル群。種別を増やすたびに空ファイルを手で用意する運用が発生する。

### 履歴保持：サーバー側に累積 SQLite

毎回のエクスポート ZIP は「その時点で HC が保持しているデータ」であって「全履歴」の保証はない。HC には自動削除設定（既定オフ／3ヶ月／18ヶ月）があり、設定変更やエクスポート失敗の連続で穴が空きうる。

そのため、サーバーに正規化済みの累積 SQLite を置き、毎回のエクスポートをレコード UUID で UPSERT する。出力はすべてこの累積 DB から生成する。

**HC 側で削除されたレコードは累積 DB から消さない。** 上流の自動削除からデータを守ることが導入目的なので、削除に追従するとその目的が失われる。

却下：毎回最新 ZIP だけを見て全量再生成する案（当初案）。実装は最小だが、HC が古いデータを消した時点で出力からも消える。

### 生データの持ち方：種別ごとのローリング窓

Drive には直近分のみを置き、全期間はサーバーの累積 DB が持つ。窓の長さは**種別ごとに設定ファイルで指定**する。血圧は数年分でも数百行だが、心拍は1ヶ月で数万行になり、事情が違いすぎるため。

却下：月別分割（`hr_2026-08` のようにタブを増やし続ける案）。全期間を Drive に置けるが、Claude が読める量を超えるタブが積み上がるだけで、実際の分析には使われない。

### 起動：1時間おきのポーリング

HC のエクスポートは**頻度（毎日/毎週/毎月）しか指定できず、実行時刻を選べない**。端末の状態次第でずれるため、時刻決め打ちの起動は外れる。

ZIP の `modifiedTime` が前回処理分より新しいときだけ処理する。新着がなければ API 1呼び出しで即終了するので、ポーリング頻度を上げてもコストはない。

### 実装言語：Go

単一の静的バイナリになり、コンテナに言語ランタイムを同梱せずに済む。「サーバーに追加インストールしたくない」という前提と一貫する。イメージは `distroless/static` ベースで10MB台に収まる。

SQLite ドライバは **`modernc.org/sqlite`（純Go実装）** を使う。cgo 版（`mattn/go-sqlite3`）の方が速いが、cgo を有効にすると静的ビルドが煩雑になる。本用途（数十万行の UPSERT と集計）では速度差が問題にならない。

**注意**：`distroless/static` には tzdata が含まれない。`_ "time/tzdata"` を import してバイナリに埋め込む。集計ロジックは `zone_offset` を使うため TZ に依存しないが、ログ表示で必要になる。

却下：Python。書き捨ての探索には向くが、今回は変換ロジックが確定したら固定であり、その利点が効かない。ランタイムの同梱が必要になる分だけ不利。

### 障害検知：`_meta` タブによる受動的な鮮度表示

能動通知（メール／ntfy 等）は入れない。`_meta` タブに最終成功時刻を書き、Claude 自身が「このデータは古い」と判断できるようにする。

**受け入れているリスク**：鍵失効などで長期間停止しても、Claude に問い合わせるまで気づかない。

## コンポーネント

外部 API に触れるのは両端の2つだけ。中間の変換ロジックは実物の ZIP を1つ `testdata/` に固定すればオフラインで全部テストできる。

| パッケージ | 責務 | 依存 |
|---|---|---|
| `internal/drivesource` | ZIP 取得。新着がなければ「なし」を返す | Drive API |
| `internal/hcreader` | エクスポート DB → 正規化レコード列。種別マッピングは宣言的設定から読む | `database/sql` のみ |
| `internal/store` | 累積 SQLite。UPSERT・期間クエリ・日次集約 | `database/sql` のみ |
| `internal/sheetssink` | タブ書き込み。無ければ `addSheet`、あれば `clear` + `update` | Sheets API |

`cmd/health-connect-converter` はこの4つを繋ぎ、`time.Ticker` でポーリングループを回すだけ。`--once` フラグで1回だけ実行して終了する。

外部 API に触る2つはインターフェースで抽象化し、テストではフェイク実装を挿す。

## データモデル（累積 SQLite）

種別ごとにテーブルを分ける（列が異なるため）。共通列を揃える。

| 列 | 内容 |
|---|---|
| `uuid` | HC のレコード UUID。PRIMARY KEY |
| `start_time` / `end_time` | UTC epoch ms |
| `zone_offset` | 記録時のタイムゾーンオフセット |
| `app_id` | データ提供元アプリ |
| （種別固有） | 血圧なら収縮期/拡張期、心拍なら bpm など |

UPSERT は `INSERT ... ON CONFLICT(uuid) DO UPDATE`。トランザクションで一括適用する。

**日付境界**：日次集計は `zone_offset` を使って「記録された現地日」に割り当てる。コンテナの `TZ` はログ表示用で、集計ロジックはこれに依存させない。

## 出力タブ

| タブ | 内容 | 範囲 |
|---|---|---|
| `daily_summary` | 1行/日。全種別の集約を横持ち（`date, bp_sys_mean, bp_dia_mean, hr_mean, hr_resting, sleep_total_min, sleep_deep_min, steps, weight_kg, ...`） | 全期間 |
| `<種別>_raw` | 種別ごとの生データ（`bp_raw`, `hr_raw`, `sleep_stages`, `steps_raw` ...） | 種別ごとの窓 |
| `_meta` | 最終成功時刻／処理した ZIP の日時／種別ごとの最新レコード日時と件数 | — |

傾向の振り返りと相関分析は `daily_summary` 1枚でほぼ完結する。生データタブはそこから深掘りするときに使う。

## 設定ファイル

種別の追加はエントリ1つ。コード変更は不要。

```yaml
types:
  blood_pressure:
    source_table: blood_pressure_record_table
    window: all
    daily: [mean, count]
  heart_rate:
    source_table: heart_rate_record_series_table
    window: 30d
    daily: [mean, min, max]
```

対象は バイタル（血圧・心拍・安静時心拍・SpO2・体温・HRV）／睡眠／活動（歩数・距離・消費カロリー・運動セッション）／身体測定（体重・体脂肪率）の4群、10〜15種別程度。

## 配備：Docker Compose

ホストに追加インストールするものは Docker のみ。既存の web サービスと同じ `docker compose up -d` 運用に揃える。

イメージは GitHub Actions でビルドし GHCR へ push する（plant-diary と同じ運用。経緯は [ADR 0001](../adr/0001-distribute-container-image-via-ghcr.md)）。

```yaml
services:
  health-connect-converter:
    image: ghcr.io/canpok1/health-connect-converter:latest
    pull_policy: always
    restart: unless-stopped
    environment:
      TZ: Asia/Tokyo
      HC_POLL_INTERVAL: "1h"     # time.ParseDuration 形式
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./secrets/sa-key.json:/run/secrets/sa-key.json:ro
      - /srv/health-connect-converter/data:/data     # 累積 SQLite と state
```

- **スケジューラはコンテナ内の常駐ループ**（`処理 → sleep` の繰り返し）。cron も systemd timer も不要で、追加バイナリがゼロになる
- `/data` は named volume ではなく **bind mount**。累積 DB が正史なので、既存のバックアップ運用にそのまま乗せられる形にする
- SA 鍵は環境変数ではなく **read-only マウント**。環境変数はログや `docker inspect` に露出する
- 手動実行は `docker compose run --rm health-connect-converter --once`。初回のスキーマ調査とバックフィルにも使う

**エラー時の挙動**：常駐ループはエラーで終了しない。ログに出して state を更新せず、次の周回でリトライする。プロセスを落とすと `restart: unless-stopped` が再起動ループを作り、かえって気づきにくくなる。

**イメージ**：マルチステージビルド。ビルド段は `golang:1`（最新安定版）、実行段は `gcr.io/distroless/static-debian12`（CA 証明書を含み、非 root で動く）。`CGO_ENABLED=0` で静的バイナリを1つ置くだけの構成になる。

**主な依存**：

| 用途 | パッケージ |
|---|---|
| SQLite | `modernc.org/sqlite` |
| Drive / Sheets | `google.golang.org/api/drive/v3`, `google.golang.org/api/sheets/v4` |
| 認証 | `google.golang.org/api/option`（SA 鍵ファイルを指定） |
| 設定 | `gopkg.in/yaml.v3` |
| tzdata 埋め込み | `_ "time/tzdata"`（標準） |

ZIP 展開は標準 `archive/zip`、ポーリングは標準 `time.Ticker` で足りる。

## Ansible

role `health_connect_export` の責務は「ファイル配置」と「compose の適用」に収まる。

1. `compose.yaml` / `config.yaml` を配置（イメージは GHCR から pull するため Dockerfile / Go ソースの配置は不要）
2. SA 鍵を Ansible Vault から復号して配置（0600）
3. `community.docker.docker_compose_v2` で `pull` してから `up -d`
4. 配置ファイルの変更時に handler で再起動

## 事前の手作業

1. Google Cloud でプロジェクト作成、Drive API と Sheets API を有効化
2. サービスアカウント作成、JSON 鍵を `gcloud` で発行
3. Drive の「バックアップ」フォルダをサービスアカウントに **閲覧者** で共有
4. 空のスプレッドシートを1個作り、サービスアカウントに **編集者** で共有
5. Android 側でヘルスコネクトの定期エクスポート（毎日／出力先は上記フォルダ）を設定

以降のタブ追加・更新は全自動。

## 実装前に確定が必要な点

**エクスポート DB のスキーマには公式ドキュメントがない。** 本設計中のテーブル名（`blood_pressure_record_table` 等）は非公式情報からの推定であり、未検証。

実装の最初のステップは、実際に1回エクスポートを走らせて `sqlite3` CLI で `.schema` を吸い出し、対象種別ごとのテーブル名・列名・単位を確定させること。ここが埋まるまで `internal/hcreader` は書けない。

この調査は手作業でよく、Go のコードを書く必要はない。確定したスキーマをそのまま設定 YAML と `testdata/` のフィクスチャに落とす。

## 未決事項

- 累積 DB のバックアップ方法（正史なので消えると痛い。既存のバックアップ対象に含めるか）
- 種別ごとの窓の具体値（実物のデータ量を見てから決める）
- スプレッドシートのセル上限（1シート1000万セル）に到達した場合の扱い
