# 0002. config.yamlをコンテナイメージに同梱する

- ステータス: 採用
- 日付: 2026-08-29

## コンテキスト

`config.yaml`（種別ごとの取り込み・集計ルール）は当初、`.env`（plant-diary）と同じ発想で、ホスト側に配置し `docker-compose.yml` からbind mountする想定だった（mini-pc-setupが`config.example.yaml`からコピー生成）。

しかし配備先はmini-pc 1台のみで、環境ごとに異なる設定を切り替える想定がない。ホスト側にファイルを置く運用は、設定を変えるたびにmini-pc-setup側の手順（Ansible role）とhealth-connect-converter側の両方を意識する必要があり、単一設定しか使わない前提には過剰だった。

## 決定

`config.yaml` を本リポジトリに直接コミットし、Dockerfileでバイナリと同じ階層へ `COPY` してイメージに同梱する。`docker-compose.yml` の bind mount と、mini-pc-setup側の生成ロジック（`config.example.yaml`からのコピー）は廃止する。

## 結果

- 良い影響: ホスト側に配置ファイルを用意する手順が丸ごと不要になり、mini-pc-setup側のroleが単純になる。設定はコードと同じPRでレビュー・履歴管理できる。
- 悪い影響: 設定を変えるだけでもイメージの再ビルド・再pushが必要になる（`docker compose restart` では反映されない）。複数環境で異なる設定を使いたくなった場合は、この前提から見直しが必要。

## 検討した代替案

- ホスト側ファイル＋bind mount（plant-diaryの`.env`と同じ方式、当初案）: 環境ごとの切り替えが容易。却下: 単一環境しか使わない前提では管理箇所が増えるだけで恩恵がない。
