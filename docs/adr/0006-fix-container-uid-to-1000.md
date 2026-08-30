# 0006. コンテナの実行UID/GIDをplant-diaryに合わせて固定する

- ステータス: 採用
- 日付: 2026-08-30

## コンテキスト

`./data:/data` は bind mount で、累積 SQLite を書き込むのはコンテナ内のプロセスである。`Dockerfile` はイメージを `distroless/static-debian12` ベースとし `USER nonroot:nonroot`（固定 UID/GID `65532`）で実行していたため、ホスト側の `data` ディレクトリがこの UID/GID で書き込み可能になっていないと `unable to open database file` で起動に失敗する。

実機（mini-pc）で確認したところ、`data` ディレクトリは Docker Compose がホスト側に自動生成した際に `root` 所有となっており、この問題が実際に発生した。

対応として、当初はコンテナの実行 UID/GID をホストの実行ユーザーに動的に合わせる案（`docker-compose.yml` の `user:` と `.env` の `HC_UID`/`HC_GID` で mini-pc-setup 側から動的に渡す）を検討・実装した。しかしこの方式は `.env`（本来アプリの設定を渡す経路、[ADR 0005](0005-deployment-identifiers-via-env-file.md)）にコンテナ実行環境専用の値が混ざる、mini-pc-setup 側の実装が増えるなど、実装コストに対して急ぎ対応する価値が見合わなかった。

同様の bind mount 構成を取る `plant-diary` は、`Dockerfile` でビルド時に固定した UID/GID（`appuser`、`adduser -D` による採番で `1000:1000`）のまま動いており、`docker-compose.yml` や `.env` には UID/GID に関する記述が一切無い。mini-pc の実行ユーザー（`tanabe`、UID/GID `1000`）とたまたま一致していたために、権限問題が表面化していなかっただけと判明した（動的に合わせる仕組みは持たない）。

## 決定

動的合わせ方式は採用せず、`plant-diary` と同じ「ビルド時に固定した UID/GID で実行する」方式に揃える。`Dockerfile` の `USER` を `nonroot:nonroot`（`65532`）から `1000:1000` に変更する。`docker-compose.yml` に UID/GID 関連の設定は置かない。

`data` ディレクトリは mini-pc-setup 側が `1000:1000` の所有で作成する。

Go アプリ側はユーザー名解決（`os/user` パッケージ等）を使っておらず、`/etc/passwd` に UID `1000` のエントリが無い状態でも数値 UID 指定だけで問題なく動作することを確認済み。

## 結果

- 良い影響: `plant-diary` と実装パターンが完全に揃い、`docker-compose.yml`・`.env` に変更が要らない。mini-pc 上の複数コンテナが同じ UID/GID で動くため、権限管理がシンプルになる。
- 悪い影響: mini-pc の実行ユーザーの UID/GID が `1000:1000` でなくなった場合（別環境への配備、ユーザー変更など）は今回と同じ問題が再発する。ホストへの動的追従を持たないため、`nonroot`（`65532`）という「rootでも一般ユーザーの慣習的なUIDでもない値」で得られていた分離も失う。UID を変更する必要が生じた場合、`Dockerfile` の再ビルド・再pushが必要になる。

## 検討した代替案

- コンテナの実行UID/GIDをホストの実行ユーザーに動的に合わせる（`docker-compose.yml` の `user:` + `.env` の `HC_UID`/`HC_GID`、mini-pc-setup 側で `ansible_facts` から取得）: UID がずれる環境でも自動的に追従できる。却下（今回は見送り）: `.env` にアプリ設定と無関係な値が混ざる、実装コストが増える。将来 UID がずれる環境への配備が必要になった場合に再検討する。
- ホスト側の `data` ディレクトリを `nonroot` の固定 UID/GID（`65532`）に `chown` する: `Dockerfile` の変更が不要。却下: `65532` というマジックナンバーを Ansible 側にハードコードすることになり、`distroless` のベースイメージ変更や UID 変更に追従できない。`plant-diary` との運用パターンも揃わない。
- `data` ディレクトリのパーミッションを `0777` にする: 実装が最小で済む。却下: 書き込み権限が必要以上に緩くなる。
