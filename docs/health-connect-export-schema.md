# ヘルスコネクト エクスポートDBのスキーマ

`docs/draft/spec.md` が「実装前に確定が必要な点」に挙げていた、エクスポート DB のスキーマ調査結果。実物のエクスポートファイル（`health_connect_export.db`、47MB、2026-08-28 取得）を読んで確認した。**公式ドキュメントは存在しないため、ここに書いたものが唯一の根拠になる。**

調査に使った手段: `modernc.org/sqlite` で読み取り専用に開き、`sqlite_master` と各テーブルのサンプル行を出力した。

## テーブルの類型

レコード種別のテーブルは、時刻の持ち方で分かれる。2026-08-28 の調査では3類型（instant / interval / series）だったが、睡眠ステージの取り込みで **segment**（親の期間＋子の区間と種別）を足した。**種別ごとに列名が違うのではなく、この類型のどれかに当てはまる。** 読み出しの定型処理は類型ごとに共通部品として持ち（`internal/hcsql`）、時刻列名は種別ごとのコードが指定する。

### instant — 瞬時値

```
row_id, uuid, last_modified_time, client_record_id, client_record_version,
device_info_id, app_info_id, recording_method, dedupe_hash,
time, zone_offset, local_date,
<種別固有の値列...>,
local_date_time INTEGER AS (time + 1000 * zone_offset),
device_data_provider_id
```

| テーブル | 値列 | 件数 |
|---|---|---|
| `blood_pressure_record_table` | `systolic` REAL, `diastolic` REAL, `measurement_location` TEXT, `body_position` TEXT | 1102 |
| `weight_record_table` | `weight` REAL | 1781 |
| `body_fat_record_table` | `percentage` REAL | 1675 |
| `basal_metabolic_rate_record_table` | `basal_metabolic_rate` REAL | 2233 |
| `height_record_table` | `height` REAL | 1 |
| `resting_heart_rate_record_table` | `beats_per_minute` INTEGER | 0 |
| `oxygen_saturation_record_table` | `percentage` REAL | 0 |
| `body_temperature_record_table` | — | 0 |
| `heart_rate_variability_rmssd_record_table` | — | 0 |

### interval — 期間

```
row_id, uuid, ..., dedupe_hash,
start_time, start_zone_offset, end_time, end_zone_offset, local_date,
<種別固有の値列...>,
local_date_time_start_time INTEGER AS (start_time + 1000 * start_zone_offset),
local_date_time_end_time   INTEGER AS (end_time + 1000 * end_zone_offset),
device_data_provider_id
```

| テーブル | 値列 | 件数 |
|---|---|---|
| `steps_record_table` | `count` INTEGER | 107077 |
| `distance_record_table` | `distance` REAL | 57867 |
| `total_calories_burned_record_table` | `energy` REAL | 10965 |
| `sleep_session_record_table` | `notes` TEXT, `title` TEXT（数値なし） | 143 |
| `exercise_session_record_table` | `exercise_type` INTEGER, `title` TEXT, `has_route` INTEGER, `session_rate_of_perceived_exertion` REAL | 227 |
| `active_calories_burned_record_table` | — | 0 |

### series — 親子

親テーブルは interval 型で**値列を持たない**。実測値は子テーブルに `parent_key`（親の `row_id`）・値列・`epoch_millis` の3列で入る。

| 親テーブル | 子テーブル | 子の値列 | 親/子の件数 |
|---|---|---|---|
| `heart_rate_record_table` | `heart_rate_record_series_table` | `beats_per_minute` INTEGER | 1102 / 1102 |
| `SpeedRecordTable` | `speed_record_table` | `speed` REAL | 21152 / 21152 |
| `CyclingPedalingCadenceRecordTable` | `cycling_pedaling_cadence_record_table` | — | 0 / 0 |
| `PowerRecordTable` | `power_record_table` | — | 0 / 0 |
| `StepsCadenceRecordTable` | `steps_cadence_record_table` | — | 0 / 0 |

**親テーブル名が CamelCase になっている種別がある**（`SpeedRecordTable` 等）。snake_case のほうが子テーブルなので取り違えに注意する。心拍だけは親が snake_case（`heart_rate_record_table`）で子が `..._series_table`。

今回のデータでは親1件あたりの子は常に1件だったが（`MAX(COUNT(*)) = 1`）、これは測定元が単発測定のアプリだからで、**1対多を前提に実装する**。

## 実測で確認した事実

- **`uuid` は 16 バイトの BLOB。** TEXT ではない。文字列として扱うには hex エンコードする（例: `8eeed20eaffa302ba456713382e81048`）
- **`zone_offset` の単位は秒。** 生成列の定義 `time + 1000 * zone_offset` がミリ秒に揃えていることから確定。今回のデータは全件 `32400`（JST）
- **`local_date` は epoch day**（1970-01-01 からの日数）。現地日が既に入っている。ただし series の子テーブルには無い
- **`app_info_id` は `application_info_table.row_id` への外部キー。** アプリのパッケージ名は `application_info_table.package_name` を JOIN して得る（例: `com.google.android.apps.fitness`、`jp.co.omron.healthcare.omron_connect`）
- **`sleep_stages_table` は 2026-08-28 時点で 0 件。** スキーマ（`parent_key`, `stage_start_time`, `stage_end_time`, `stage_type`）は存在するがデータが無かった。2026-09-12 のエクスポートでは Fitbit が書いている（後述の再調査を参照）
- データの範囲は `local_date` で 19777〜20694（2024-02-24 〜 2026-08-28）の 918 日ぶん

## 値の単位

**ヘルスコネクトは内部表現で保持しており、そのままでは読めない値がある。** 設定ファイルに倍率（`scale`）を持たせて変換する。

| 種別 | 列 | 内部単位 | 実測値の例 | 目的の単位 | 倍率 |
|---|---|---|---|---|---|
| 血圧 | `systolic` / `diastolic` | mmHg | 125 / 68 | mmHg | 1 |
| 体重 | `weight` | **グラム** | 64000 | kg | 0.001 |
| 体脂肪率 | `percentage` | % | 20.8 | % | 1 |
| 身長 | `height` | メートル | 1.67 | m | 1 |
| 歩数 | `count` | 歩 | 14 | 歩 | 1 |
| 距離 | `distance` | メートル | 1.26 | m | 1 |
| 消費エネルギー | `energy` | **カロリー** | 749366.5 | kcal | 0.001 |
| 基礎代謝 | `basal_metabolic_rate` | **ワット** | 70.5 | kcal/日 | 20.65 |
| 心拍 | `beats_per_minute` | bpm | 76 | bpm | 1 |

`basal_metabolic_rate` はワット（Power）で保持されており、kcal/日 へは 86400（1日の秒数）÷ 4184（1kcalのジュール数）≈ 20.65 倍が要る。単純な倍率で扱え意味も明確なため、対象種別に含める（値名は `kcal_per_day` として単位を明示する）。

## spec.md との差分

`docs/draft/spec.md` が非公式情報から推定していた内容のうち、実物と食い違っていた点。

- `heart_rate_record_series_table` を心拍の `source_table` としていたが、これは**子テーブル**で `uuid` を持たない。親は `heart_rate_record_table`
- レコードの共通列を `start_time` / `end_time` / `zone_offset` としていたが、instant 型は `time` / `zone_offset` の2列しか持たない。interval 型はオフセットを開始・終了で別々に持つ
- 出力タブ案にある `sleep_deep_min` と `sleep_stages` は、`sleep_stages_table` が空のため出せなかった。睡眠はセッションの長さのみ扱っている（2026-09-12 の再調査で状況が変わった。後述）
- 種別の値をそのまま出力できる前提だったが、体重（グラム）と消費エネルギー（カロリー）は単位変換が要る

## 2026-09-12 の再調査（Fitbit 接続後）

実物のエクスポート（`health_connect_export.db`、53MB、2026-09-11 取得）を同じ手段で読み直した。**2026-09-09 から Fitbit（`com.fitbit.FitbitMobile`）が書き始めており、上の調査時点で空だったテーブルにデータが入っている。**

取り込み対象に追加したもの（どちらも instant 型・`category: vitals`・単位変換なし）。

| テーブル | 値列 | 件数 | 期間 | 実測値 |
|---|---|---|---|---|
| `heart_rate_variability_rmssd_record_table` | `heart_rate_variability_millis` REAL | 178 | 2026-09-09〜09-11 | 11.2〜66.5 ms、平均 40.9。睡眠中のみ5分間隔（1日約86件）なので窓は `30d` |
| `respiratory_rate_record_table` | `rate` REAL | 2 | 2026-09-10〜09-11 | 14.6 / 14.8 回/分。1日1件なので窓は `all` |

データはあるが**取り込まなかった**もの。

- **`exercise_segments_table`（243件、`exercise_session_record_table` の子）** — 全列が distinct = 1（`segment_type` は 24 のみ、`repetitions_count` は 0 のみ、`weight_grams` は全 NULL、`rate_of_perceived_exertion` は `session_rate_of_perceived_exertion` と同じ壊れ値 1.4e-45）。情報がゼロなので列を足す意味がない
- ~~**`sleep_stages_table`（138件、2セッション、Fitbit のみ）**~~ → **2026-09-12 に `sleep_stage` 種別として取り込んだ**（[ADR 0011](adr/0011-sleep-stage-with-own-times.md)）。子テーブルの形が既存の series 型（`parent_key` + 値列 + `epoch_millis`）と違い（`stage_start_time` / `stage_end_time` / `stage_type`）、集計も「種別ごとの合計時間」になるため、読み出しに segment 型を足した。実測は 2026-09-10 が 覚醒21.5 / 浅い261.5 / 深い31.5 / REM106.5 分、2026-09-11 が 覚醒55.5 / 浅い299 / 深い34 / REM92.5 分で、どちらも合計が睡眠時間と一致する
- 上記以外でデータを持つ未登録テーブルは、ヘルスコネクト自身の管理用（`activity_date_table` / `change_log_request_table` / `read_access_logs_table` / `preference_table` / `device_info_table` / `application_info_table` / `health_data_category_priority_table` / `device_data_sources_table` / `device_data_provider_metadata_table` / `android_metadata`）で、健康データではない

`skin_temperature_record_table` や `vo2_max_record_table` など Fitbit が将来書きうるテーブルは現時点で 0 件。`_meta` タブの `export_<テーブル名>_rows` に全テーブルの行数が出るため、書き始めればそこで気付ける。
