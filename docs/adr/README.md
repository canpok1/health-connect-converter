# Architecture Decision Records (ADR)

重要度の高い判断を記録する。フォーマットは MADR 軽量版（日本語本文）。

| 番号 | タイトル | ステータス | 日付 |
|------|----------|------------|------|
| [0001](0001-distribute-container-image-via-ghcr.md) | コンテナイメージをGHCR経由で配布する | 採用 | 2026-08-29 |
| [0002](0002-bundle-config-into-image.md) | config.yamlをコンテナイメージに同梱する | 採用 | 2026-08-29 |
| [0003](0003-relative-data-volume-path.md) | 累積データの保存先を相対パス（gitクローン先内）にする | 採用 | 2026-08-29 |
| [0004](0004-config-driven-generic-records.md) | 種別を汎用レコードで扱い、累積DBのスキーマをconfig.yamlから生成する | 採用 | 2026-08-29 |
| [0005](0005-deployment-identifiers-via-env-file.md) | デプロイ固有の識別子は.envの環境変数で渡す | 採用 | 2026-08-29 |
