# 0005. デプロイ固有の識別子は.envの環境変数で渡す

- ステータス: 採用
- 日付: 2026-08-29

## コンテキスト

実装には Drive の監視フォルダID と 出力先スプレッドシートID が要る。[ADR 0002](0002-bundle-config-into-image.md) で `config.yaml` はリポジトリにコミットしイメージへ同梱すると決めたため、素直に読むとこの2つのIDも `config.yaml` に書くことになる。

しかし本リポジトリと `docker-compose.yml` は公開されており、IDを直接書くと Drive フォルダとスプレッドシートの所在が公開される。秘密情報ではない（共有設定が無ければ読めない）が、公開する必然もない。加えて ADR 0002 が受け入れた「設定変更にイメージ再ビルドが要る」というコストは、種別ルールなら妥当でも、接続先IDに及ぶのは不便が勝つ。

## 決定

`config.yaml` には種別ごとの取り込み・集計ルールのみを置き、デプロイ固有の値は環境変数で渡す。ホスト側の `.env`（gitignore 対象）に置き、`docker-compose.yml` からは `${...}` で参照する。リポジトリには `.env.example` をコミットする。

| 環境変数 | 内容 | 既定 |
|---|---|---|
| `HC_DRIVE_FOLDER_ID` | 監視する Drive フォルダID | （必須） |
| `HC_SPREADSHEET_ID` | 出力先スプレッドシートID | （必須） |
| `HC_SA_KEY_PATH` | サービスアカウント鍵 | `/run/secrets/sa-key.json` |
| `HC_DB_PATH` | 累積SQLite | `/data/health.db` |
| `HC_CONFIG_PATH` | 設定ファイル | `/app/config.yaml` |
| `HC_POLL_INTERVAL` | ポーリング間隔 | `1h` |
| `HC_LOG_LEVEL` | ログレベル | `info` |

`.env` の配置は `mini-pc-setup` 側の Ansible role が Vault から行う（SA鍵と同じ経路）。

## 結果

- 良い影響: 公開リポジトリにIDが載らない。接続先の変更が `.env` 編集と `docker compose up -d` だけで済み、イメージ再ビルドが要らない。`plant-diary` の `.env` 運用と揃う。
- 悪い影響: ADR 0002 が達成した「ホスト側に配置ファイルを用意する手順が丸ごと不要」が部分的に戻る。SA鍵の配置が既に必要なので新たな手順区分は増えないが、mini-pc-setup 側の role が扱うファイルは2つになる。

## 検討した代替案

- IDも `config.yaml` に書く（ADR 0002 の素直な適用）: 設定が1ファイルに集まり、ホスト側の配置物が鍵だけで済む。却下: 公開リポジトリにIDが載り、接続先の変更にイメージ再ビルドが要る。
- `docker-compose.yml` の `environment` に直接IDを書く: `.env` を増やさずに済む。却下: compose ファイル自体がリポジトリにコミットされるため、公開される点は `config.yaml` と変わらない。
- IDも SA鍵と同じくファイルとしてマウントする: 秘密情報と同じ扱いで一貫する。却下: 秘密ではない値に読み取り専用マウントを増やすのは過剰で、環境変数で足りる。
