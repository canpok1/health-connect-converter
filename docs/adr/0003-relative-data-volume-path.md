# 0003. 累積データの保存先を相対パス（gitクローン先内）にする

- ステータス: 採用
- 日付: 2026-08-29

## コンテキスト

当初 `docker-compose.yml` は累積SQLiteの保存先を `/srv/health-connect-converter/data:/data` としていた（`plant-diary` の `./data` とは異なり、gitクローン外の絶対パスを選んでいた）。

実機（mini-pc）で `make deploy` したところ、`error while creating mount source path '/srv/health-connect-converter/data': mkdir /srv/health-connect-converter: read-only file system` で失敗した。mini-pcのDockerはsnapパッケージ版で、AppArmorの confinement により `$HOME` 配下以外へのbind mountができないことが原因。

## 決定

`plant-diary` と同じく、保存先を `./data`（gitクローン先ディレクトリの直下、`$HOME/src/health-connect-converter/data`）に変更する。

ただし `mini-pc-setup` 側の `ansible.builtin.git`（`force: yes`）は再クローン時にgit管理外ファイルを削除しうるため、`plant-diary.yml` と同様に、クローン前に `data/` をローカルへバックアップするタスクを追加する（再クローンのたびにバックアップを取るだけで、自動復元はしない。plant-diaryと同じ運用に揃える）。

## 結果

- 良い影響: snapのAppArmor制限を回避できる。plant-diaryと運用が揃う。
- 悪い影響: 累積DBが本リポジトリのgitクローン先ディレクトリの中に置かれるため、再デプロイのたびに（バックアップはあるが）データ消失のリスクをゼロにはできない。`/srv` のようなgit管理と独立した場所に置く場合より安全性は落ちる。

## 検討した代替案

- `$HOME` 配下だがgitクローン外の固定パス（例: `~/data/health-connect-converter`）: git cloneのforce:yesによる消失リスクを避けられる。却下: 今回はplant-diaryとの運用一貫性を優先し、まずは同じ方式に揃えた。将来データ消失が実際に問題になった場合はこちらへの移行を検討する。
- snapを使わない一般的なDockerパッケージへの入れ替え: `/srv` のような任意パスへのbind mountが可能になる。却下: mini-pcの他サービス（cloudflared・plant-diary）に影響する変更で、今回のスコープを超える。
