# 0001. コンテナイメージをGHCR経由で配布する

- ステータス: 採用
- 日付: 2026-08-29

## コンテキスト

`docs/draft/spec.md` では自宅サーバー1台構成を前提に「レジストリを使わずローカルビルド」（Ansibleで `docker compose up -d --build` する運用）としていた。今回コンテナ生成処理を追加するにあたり、姉妹プロジェクトplant-diaryと運用を揃えたいという要望があり、方針を再検討した。

## 決定

plant-diaryと同様、GitHub Actionsでイメージをビルドし GHCR (ghcr.io) へ push する。`docker-compose.yml` は `image: ghcr.io/canpok1/health-connect-converter:latest` を pull する構成にする。

## 結果

- 良い影響: CIで検証済みのイメージをそのままデプロイでき、サーバー側でのビルドが不要になる。plant-diaryと運用手順（`docker compose pull && up -d`）が揃う。
- 悪い影響: spec.mdが前提としていた「ホストへの追加インストールはDockerのみ」「レジストリ不使用」という制約から外れる。GHCRへの到達性（インターネット接続）がサーバー側に必要になる。

## 検討した代替案

- ローカルビルド（spec.mdの元案）: サーバー1台構成に最小限で、レジストリ依存がない。却下: plant-diaryとの運用一貫性を優先。
- ハイブリッド（CIでビルド検証のみ・pushはしない）: レジストリ不使用の制約は守れるが、サーバー側で結局ビルドが必要でCIの検証と二重管理になる。却下: 二重管理の手間に見合うメリットが薄い。
