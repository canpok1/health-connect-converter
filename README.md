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

GitHub Actions で main ブランチへの push をトリガーにコンテナイメージをビルドし、GHCR (`ghcr.io/canpok1/health-connect-converter`) へ push する。配備先（mini-pc）でのサービス起動は [mini-pc-setup](https://github.com/canpok1/mini-pc-setup) が管理する compose.yml で行うため、本リポジトリに `docker-compose.yml` は置かない。

詳細な運用方針は [docs/draft/spec.md](docs/draft/spec.md#配備docker-compose) を参照。
