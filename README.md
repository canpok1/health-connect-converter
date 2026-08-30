# health-connect-converter

ヘルスコネクトのエクスポートデータを変換するツール。設計は [docs/draft/spec.md](docs/draft/spec.md) を参照。

## セットアップ

```bash
make setup
```

## 実行

```bash
make run
```

## 開発

```bash
make lint
make test
make build
```

## デプロイ

GitHub Actions で main ブランチへの push をトリガーにコンテナイメージをビルドし、GHCR (`ghcr.io/canpok1/health-connect-converter`) へ push する。

起動前の準備:

1. `.env.example` を `.env` にコピーし、`HC_DRIVE_FOLDER_ID` と `HC_SPREADSHEET_ID` を埋める。`HC_UID` / `HC_GID` には `data` ディレクトリを所有させるホスト側ユーザーの UID/GID（`id -u` / `id -g`）を設定する
2. `data` ディレクトリを 1 で設定した UID/GID の所有で作成する
3. サービスアカウント鍵を `secrets/sa-key.json` に置き `chmod 600` する

必要な環境変数の一覧は [ADR 0005](docs/adr/0005-deployment-identifiers-via-env-file.md)、`HC_UID` / `HC_GID` の経緯は [ADR 0006](docs/adr/0006-container-uid-matches-host-user.md) を参照。

配備先では以下で起動する。

```bash
docker compose pull
docker compose up -d
```

手動実行（初回のスキーマ調査・バックフィルなど）:

```bash
docker compose run --rm health-connect-converter --once
```

ログの確認:

```bash
docker compose logs -f
```

詳細な運用方針は [docs/draft/spec.md](docs/draft/spec.md#配備docker-compose) を参照。
