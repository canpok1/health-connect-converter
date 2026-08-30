# 0006. コンテナの実行UID/GIDをホストの実行ユーザーに合わせる

- ステータス: 採用
- 日付: 2026-08-30

## コンテキスト

`./data:/data` は bind mount で、累積 SQLite を書き込むのはコンテナ内のプロセスである。`Dockerfile` はイメージを `distroless/static-debian12` ベースとし `USER nonroot:nonroot`（固定 UID/GID `65532`）で実行するため、ホスト側の `data` ディレクトリがこの UID/GID で書き込み可能になっていないと `unable to open database file` で起動に失敗する。

実機（mini-pc）で確認したところ、`data` ディレクトリは Docker Compose がホスト側に自動生成した際に `root` 所有となっており、この問題が実際に発生した。

同様の bind mount 構成を取る `plant-diary`（`appuser`、UID/GID `1000`）は、たまたま mini-pc の実行ユーザー（`tanabe`、UID/GID `1000`）と一致していたために問題が表面化していなかった。これは環境依存の偶然であり、実行ユーザーの UID が異なる環境では同じ理由で壊れる。

## 決定

`docker-compose.yml` にコンテナの実行ユーザーを指定する `user: "${HC_UID:?HC_UID is required}:${HC_GID:?HC_GID is required}"` を追加する。`HC_UID` / `HC_GID` はホスト側のデプロイ実行ユーザーの UID/GID を `.env` 経由で渡す（[ADR 0005](0005-deployment-identifiers-via-env-file.md) と同じ経路。値の生成は `mini-pc-setup` 側の Ansible role が `ansible_facts` から動的に取得する）。

`data` ディレクトリもホスト側の実行ユーザー自身の所有として作成する（`mini-pc-setup` 側の責務。Docker Compose の自動生成に任せない）。

## 結果

- 良い影響: ホスト・コンテナ双方が同じ UID/GID で揃うため、bind mount の書き込み権限が常に一致する。`nonroot` の固定 UID（`65532`）をホスト側の設定に直書きせずに済む。ディレクトリのパーミッションを緩める（`0777` 等）必要もない。
- 悪い影響: `HC_UID` / `HC_GID` という新しい必須環境変数が増える。`Dockerfile` の `USER nonroot:nonroot` はイメージ内の既定値としては残るが、`docker compose up` 時は常に上書きされるため、`.env` を経由しない `docker run` 等で直接使うとホスト側の権限と噛み合わない可能性がある。

## 検討した代替案

- ホスト側の `data` ディレクトリを `nonroot` の固定 UID/GID（`65532`）に `chown` する: 変更箇所は `mini-pc-setup` 側だけで完結する。却下: `65532` というマジックナンバーを Ansible 側にハードコードすることになり、`distroless` のベースイメージ変更や UID 変更に追従できなくなる。
- `data` ディレクトリのパーミッションを `0777` にする: 実装が最小で済む。却下: 書き込み権限が必要以上に緩くなる。
- 何もしない（`plant-diary` と同様、host user の UID がたまたま一致することに期待する）: 追加実装が不要。却下: mini-pc の実行ユーザーが変わる、または別環境へ配備する場合に同じ障害が再発する。今回すでに実際の障害として発生しており、看過できない。
