# ドメインモデル

[ADR 0012](adr/0012-split-measured-data-from-aggregation-model.md) で決めた層の分け方を、種別ごとの実体まで具体化したもの。実装の指針であり、ここと食い違う実装は直す。

## 3つの層

```
エクスポートDB
  │  種別ごとの読み出し（親子の結合・アプリ名の解決・単位変換をここで済ませる）
  ▼
① 保存用のモデル ── 種別ごと。累積DBに保存する形。生データタブの元
  │  種別ごとの変換（集計に必要な共通部分を取り出す）
  ▼
② 集計用のモデル ── 共通。保存しない
  │  共通の処理（重複排除 → 日次集計）
  ▼
③ サマリーモデル ── 日次の集計結果。保存せず毎回作る。daily_summary の元
```

**層を分ける目的は、変更の理由を分けること。** 上流の表が変われば種別ごとの読み出しだけ、保存の都合なら①だけ、集計の都合なら②③だけが変わる。

### 上流の表の形は層として持たない

エクスポートDBの表を忠実に写した型は作らない。読み出し関数の中で結合結果をそのまま①へ読む。

理由は3つ。単純な種別（血圧・体重）では写しと①の差がアプリ名の解決と単位変換だけで、型を分ける釣り合いが取れない。親子構造の種別（心拍・速度・睡眠ステージ）は結合して読めばそのまま①の形（1行＝1測定値／1区間）になり、親の型と子の型を作って突き合わせるほうが遠回りになる。そして上流の表の形は [docs/health-connect-export-schema.md](health-connect-export-schema.md) が正本として持っている。

上流が変わったときの影響は、種別ごとの読み出し関数に閉じる。

### ① 保存用のモデル

累積DBに保存する形。エクスポートDBから読むときに次を済ませる。

- 親子を結合して畳む（心拍・速度は「1行＝1測定値」、睡眠ステージは「1行＝1区間」）
- 記録元アプリをパッケージ名にする（エクスポートDBはアプリIDで持つ）
- 単位を意味のあるものへ直す（後述）
- 使わない列を落とす

生データタブ（`<種別>_raw`）はこの層から直接出す。種別固有の列をそのまま出せるようにするため。

### ② 集計用のモデル

①から集計に必要な共通部分を取り出したもの。**種別によらず同じ形**で、次だけを持つ。

| 項目 | 用途 |
|---|---|
| 現地日 | どの日に数えるか。①の時刻から決める（種別ごとの変換の責任） |
| 期間（開始・終了） | 重複排除でアプリ間の重なりを測る |
| 記録元アプリ | 重複排除で優先度を引く |
| 値の集まり（名前→数値） | 合計・平均・最小・最大・件数の対象 |

共通の形にしているのは、重複排除と日次集計が値の意味を見ずに動くため。ここを種別ごとにすると、この2つの処理を種別の数だけ書くことになる。

### ③ サマリーモデル

1日1行の集計結果。列名は `<種別キー>_<値名>_<関数>`（例: `sleep_stage_deep_min_sum`）。保存せず毎回作る。

## 種別カタログ

`列名←元の列` の形で、①が持つ値とエクスポートDBの列の対応を示す。倍率は単位変換。

| 種別キー | エクスポートDBの元テーブル | 時刻の形 | ①が持つ値 | 現地日 | 重複排除 | カテゴリ | 生データの窓 | 日次 |
|---|---|---|---|---|---|---|---|---|
| `basal_metabolic_rate` | `basal_metabolic_rate_record_table` | 瞬間 | `kcal_per_day←basal_metabolic_rate ×20.65` | 測定時刻 | — | body_measurements | 全期間 | mean,count |
| `blood_pressure` | `blood_pressure_record_table` | 瞬間 | `systolic←systolic` / `diastolic←diastolic` | 測定時刻 | — | vitals | 全期間 | mean,min,max,count |
| `body_fat` | `body_fat_record_table` | 瞬間 | `percent←percentage` | 測定時刻 | — | body_measurements | 全期間 | mean,min,max,count |
| `distance` | `distance_record_table` | 期間 | `m←distance` | 開始 | あり | activity | 30日 | sum |
| `exercise_session` | `exercise_session_record_table` | 期間 | `duration_min`（期間から計算）／`exercise_type`（保存のみ、出力しない） | 開始 | あり | activity | 全期間 | sum,count |
| `heart_rate` | `heart_rate_record_table` + `heart_rate_record_series_table` | 期間＋連続測定 | `bpm←beats_per_minute` | 測定時刻 | — | vitals | 1日 | mean,min,max,count |
| `heart_rate_variability` | `heart_rate_variability_rmssd_record_table` | 瞬間 | `rmssd_ms←heart_rate_variability_millis` | 測定時刻 | — | vitals | 30日 | mean,min,max,count |
| `height` | `height_record_table` | 瞬間 | `cm←height ×100` | 測定時刻 | — | body_measurements | 全期間 | mean,count |
| `oxygen_saturation` | `oxygen_saturation_record_table` | 瞬間 | `percent←percentage` | 測定時刻 | — | vitals | 全期間 | mean,min,max,count |
| `respiratory_rate` | `respiratory_rate_record_table` | 瞬間 | `breaths_per_min←rate` | 測定時刻 | — | vitals | 全期間 | mean,count |
| `resting_heart_rate` | `resting_heart_rate_record_table` | 瞬間 | `bpm←beats_per_minute` | 測定時刻 | — | vitals | 全期間 | mean,min,max,count |
| `sleep` | `sleep_session_record_table` | 期間 | `duration_min`（期間から計算） | **終了（起床日）** | あり | sleep | 全期間 | sum,count |
| `sleep_stage` | `sleep_session_record_table` + `sleep_stages_table` | 期間＋区間の種別 | 区間の開始・終了・ステージ種別・**親セッションの終了時刻**。集計では長さを `awake_min` / `light_min` / `deep_min` / `rem_min` へ振り分ける | **親セッションの終了（起床日）** | — | — | 30日 | sum,count |
| `speed` | `SpeedRecordTable` + `speed_record_table` | 期間＋連続測定 | `m_per_s←speed` | 測定時刻 | — | activity | 30日 | mean,max,count |
| `steps` | `steps_record_table` | 期間 | `count←count` | 開始 | あり | activity | 30日 | sum |
| `total_calories_burned` | `total_calories_burned_record_table` | 期間 | `kcal←energy ×0.001` | 開始 | あり | activity | 全期間 | sum |
| `weight` | `weight_record_table` | 瞬間 | `kg←weight ×0.001` | 測定時刻 | — | body_measurements | 全期間 | mean,min,max,count |

- **カテゴリ**は重複排除でアプリの優先度を引くときに使う（ヘルスコネクトの優先度はカテゴリ単位。[ADR 0009](adr/0009-dedupe-by-health-connect-app-priority.md)）。重複排除をしない種別には不要
- `sleep_stage` の重複排除を「—」にしているのは、現状ステージを書くアプリが Fitbit だけで、全区間が同じ親に属するため既存の重なり判定と噛み合わないから（[ADR 0011](adr/0011-sleep-stage-with-own-times.md)）
- **`sleep_stage` の生データタブだけ列の形が違う。** `local_date` / `local_start` / `local_end` / `app_id` / `stage`（awake / light / deep / rem）/ `minutes` で出す。ステージ種別ごとに4列へ散らすより、ひと晩の推移が読みやすいため。日次集計は種別ごとの列に分かれる
- **`sleep_stage` の日ごと置き換えは親セッションの終了日で区切る。** 区間の開始日で区切ると、日付をまたぐひと晩が2日に割れる

## 単位

ヘルスコネクトは内部表現で保持しており、そのままでは読めない値がある。①へ読み込むときに直す。

| 種別 | 内部の単位 | 実測値の例 | ①の単位 | 倍率 |
|---|---|---|---|---|
| 体重 | グラム | 64000 | kg | 0.001 |
| 消費エネルギー | カロリー | 749366.5 | kcal | 0.001 |
| 基礎代謝 | ワット | 70.5 | kcal/日 | 20.65 |
| 身長 | メートル | 1.67 | cm | 100 |

残り（血圧の mmHg、体脂肪率の %、歩数、距離のメートル、心拍の bpm、HRV のミリ秒、呼吸数、速度の m/秒）は変換しない。

## 取り込まない情報

エクスポートDBにあるが①へ持ち込まないもの。**判断の根拠は実データの分布**（[docs/health-connect-export-schema.md](health-connect-export-schema.md)）。

| 種別 | 列 | 理由 |
|---|---|---|
| 血圧 | `measurement_location` / `body_position` | 全1,120件が `0`（未設定） |
| 睡眠 | `notes` / `title` | メモは全件空、タイトルは「睡眠分析」のみ |
| 運動 | `notes` / `title` / `has_route` / `session_rate_of_perceived_exertion` | メモとタイトルは空、経路有無は全件0、強度は壊れ値（1.4e-45） |
| 運動の区間 | `exercise_segments_table` 全体 | 全列が distinct = 1 で情報量がゼロ |

**運動の `exercise_type` は例外で、①に持つが出力しない。** 現状は全201件が同じ値（53）で列にする意味がないが、壊れているわけではなく、散歩と筋トレを分けて記録するようになれば意味を持つ。累積DBは上流が消してもデータを守るのが目的なので、保存だけしておく。

## 変えない約束

**累積DBのテーブル名・列名・単位は現状のまま**にする。テーブル名は `record_<種別キー>`。既存データをそのまま引き継ぎ、移行前後で出力が一致することを差分ゼロで確かめるため（新設した `sleep_stage` と、`exercise_session` に足した `exercise_type` は例外）。

2026-09-12 の移行では、実物のエクスポート（16種別・約30万件）に対して移行前後の出力を突き合わせ、18タブすべてがバイト一致することを確認した。以降の変更でも、合成データの期待値テスト（`internal/e2e`）が出力の意図しない変化を落とす。
