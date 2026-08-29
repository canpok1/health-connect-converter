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

GitHub Actions で main ブランチへの push をトリガーにコンテナイメージをビルドし、GHCR (`ghcr.io/canpok1/health-connect-converter`) へ push する。配備先では以下で起動する。

```bash
docker compose pull
docker compose up -d
```

詳細な運用方針は [docs/draft/spec.md](docs/draft/spec.md#配備docker-compose) を参照。
