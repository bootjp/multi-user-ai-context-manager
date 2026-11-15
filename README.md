# multi-user-ai-context-manager

複数人が使うエージェント間で、採用済み構成、改善案、進行中の変更、計測結果、TODO、ブロッカーを共有するHTTPサービスです。
Goの単一バイナリとPostgreSQLで動作します。
ローカル環境で動作確認済みのMVPです。
ライセンスは [Apache License 2.0](LICENSE) です。
[GitHubの公開リポジトリ](https://github.com/bootjp/multi-user-ai-context-manager)で管理しています。
クラウドへの配置は行っていません。
CLI、SKILL、ComposeのプロジェクトIDは `multi-user-ai-context-manager` に統一しています。

## ローカルで試す

DockerエンジンとPython 3を使える状態で、次を実行します。
初回はランダムなDBパスワードと専用のlocal-demoプロジェクトを作り、接続情報を `work/local/` の非公開ファイルへ保存します。
再実行時には既存のDBと秘密値を保持します。

```sh
git clone https://github.com/bootjp/multi-user-ai-context-manager.git
cd multi-user-ai-context-manager
./scripts/dev-up.sh
# 表示されたURL（既定は http://127.0.0.1:8080/ ）を開く
. ./work/local/multi-user-ai-context-manager.env
python3 skills/multi-user-ai-context-manager/scripts/request.py GET --project-path /snapshot
```

管理画面へは `work/local/admin.token` の値で接続できます。
作成済みlocal-demoの管理には `work/local/project.json` 内のプロジェクト管理者トークンを使います。
これらの秘密値は共有ログへ貼らず、Gitへ追加しないでください。
通常作業用の `multi-user-ai-context-manager.env` はeditor権限です。

停止は `docker compose stop`、再開は `./scripts/dev-up.sh` です。
管理者と参加者のトークンには期限があるため、期限切れの場合は通常の発行または明示的な復旧手順を使います。

## 旧ローカル環境からの移行

旧Composeの `agent-context` を使用していた場合、初回は専用の移行手順を実行します。
新しい空のDBを誤って使い始めないよう、通常の起動スクリプトは旧volumeを検出すると停止します。

```sh
# 読取りだけで対象コンテナ、全テーブルの件数、移行計画を確認
python3 scripts/migrate-local-name.py
# 内容を確認して適用
python3 scripts/migrate-local-name.py --apply
```

移行は旧アプリケーションを停止し、全DBの論理バックアップと `.env`、`work/local/` のコピーを非公開の `work/backups/name-migration-*/` へ保存します。
新しいComposeプロジェクトとvolumeへ復元し、全テーブルの件数と内容のハッシュを照合します。
復元epochを更新するため、旧カーソルとleaseは再同期が必要です。
復元手順に従い、古い冪等応答キャッシュは破棄します。
トークンと記録、外部ジョブIDを保持し、旧コンテナと旧volumeは停止状態で残します。
失敗した移行を自動で重ねて適用せず、既に新volumeがある場合は停止します。

元の `.env`、`admin.token`、`project.json`、`session.json`、`session.env` は書き換えません。
新しい接続変数を使う `work/local/multi-user-ai-context-manager.env` を別途作成します。
既存の `AGENT_CONTEXT_*` 変数も読取り互換として扱い、新しい `MULTI_USER_AI_CONTEXT_MANAGER_*` 変数があればそちらを優先します。

## Agent向けSKILLのインストール

[skills/multi-user-ai-context-manager/SKILL.md](skills/multi-user-ai-context-manager/SKILL.md) は、参加、版管理、TODO引受け、leaseの更新、外部ジョブ確認、切断復帰、ハンドオフを扱います。
共有された文章を命令や新しい実行許可として扱わない境界も含めています。
Python 3の標準ライブラリだけで使えるHTTPヘルパーを同梱し、サーバー用CLIのインストールは必須にしません。

このチェックアウトから、利用するAgentのスキルディレクトリへコピーします。
既存の同名スキルがある場合は上書きせず終了します。

```sh
# 既定は ${CODEX_HOME:-$HOME/.codex}/skills/multi-user-ai-context-manager
./scripts/install-skill.sh

# 別のスキルディレクトリを使うAgentでは保存先を指定
./scripts/install-skill.sh "$HOME/.agents/skills"
```

スキルを利用できるセッションへ接続URL、プロジェクトID、専用トークンを渡し、`$multi-user-ai-context-manager` を指定して作業します。
ローカルの接続設定には上記の `work/local/multi-user-ai-context-manager.env` を利用できます。
インストールしたスキル自体にはトークンを保存しません。
スキルの導入は、外部ベンチ、デプロイ、管理者操作の実行許可を追加しません。

## 起動

Docker Composeを利用する場合は、最初にパスワードを設定します。
DBのポートはホストへ公開せず、APIはlocalhostだけへ公開します。

```sh
cp .env.example .env
# .env の POSTGRES_PASSWORD を openssl rand -hex 32 などで生成した値に置換
# .env をGitへ追加しない
chmod 600 .env
docker compose up -d db
docker compose build app
docker compose run --rm app migrate
docker compose run --rm -it app bootstrap
# 表示されたインスタンス管理者トークンを安全な場所へ保存
docker compose up -d app
curl --fail http://127.0.0.1:8080/readyz
```

管理画面は `http://127.0.0.1:8080/` です。
管理者トークンでプロジェクトを作成し、返されたプロジェクト管理者トークンへ切り替えると、参加者のトークンを発行できます。
秘密値は発行直後にだけ表示し、画面のメモリだけに保持します。

Goから起動する場合は、専用のPostgreSQLデータベースを用意します。

```sh
go build -trimpath -o bin/multi-user-ai-context-manager ./cmd/multi-user-ai-context-manager
export DATABASE_URL='postgres://user:password@127.0.0.1:5432/multi_user_ai_context_manager?sslmode=disable'
bin/multi-user-ai-context-manager migrate
bin/multi-user-ai-context-manager bootstrap
bin/multi-user-ai-context-manager serve
```

`bootstrap` は未初期化のDBにだけ実行できます。
TTY以外で秘密値を受け取る場合は `bootstrap --output-file /private/path/admin.token` を明示します。
ファイルは新規作成のみ、モード0600です。
管理者トークンを起動ログへ出力する処理や公開setup APIはありません。

## 最初のプロジェクトと参加者

ローカルCLIは管理用バイナリと同じものです。
`project create` と `token issue` はHTTPを呼び、DBへ直接アクセスしません。
新しい操作には新しいキーを使い、再送では同じキーを再利用します。

```sh
export MULTI_USER_AI_CONTEXT_MANAGER_URL=http://127.0.0.1:8080
# 秘密値はシェル履歴や共有ログへ残さず、秘密管理機構などから設定する
export MULTI_USER_AI_CONTEXT_MANAGER_TOKEN='<instance-admin-token>'
bin/multi-user-ai-context-manager project create --name team --key project-team-1
# 応答の id と token を安全に受け取り、以下を設定
export MULTI_USER_AI_CONTEXT_MANAGER_PROJECT='<project-id>'
export MULTI_USER_AI_CONTEXT_MANAGER_TOKEN='<project-admin-token>'
bin/multi-user-ai-context-manager token issue --project "$MULTI_USER_AI_CONTEXT_MANAGER_PROJECT" --name alice/session-1 --role editor --key alice-session-1
```

各セッションへ渡す情報は、接続URL、プロジェクトID、専用トークンです。
参加規約とAPI例は [docs/participation.md](docs/participation.md)、契約は [docs/openapi.json](docs/openapi.json) を参照してください。
トークンの期限は通常7日、管理者は30日です。
APIでは `expires_at` を指定できます。

## 提供する操作

- コンテキストの版管理、履歴参照、日本語を含む部分一致検索、archive、監査付きredact。
- coordinator以上による不変版のpinと、参照元が更新された要約のstale表示。
- Repeatable Readのsnapshotと、バイト数制限付きcontext-pack。
- TODOの依存DAG、証跡付きブロッカー解消、期限付き作業引受け。
- 名前付き実行枠、世代番号と秘密値によるfencing、外部ジョブのcheckpoint、復旧確認。
- プロジェクト別トークンのハッシュ保存、期限、失効、rotate、管理操作の監査。
- 永続イベント、ページ再送、SSE、切断復帰するCLI watch、復元epoch。

更新は全てプロジェクト行をロックした一つのトランザクションで直列化し、状態、履歴、イベント、冪等応答を同時にcommitします。
編集には `If-Match: "<version>"`、全ての変更要求には `Idempotency-Key` が必要です。
pinの初回作成には `If-None-Match: *`、以後の更新にはpin自身の `If-Match` が必要です。
同じキーと異なる要求には409、古い版には412を返します。
redactは全版の本文を置換したうえで現在版を進め、記録をarchiveし、削除前のキャッシュからの書込みを拒否します。

トークンとleaseの秘密値は、冪等応答にも保存しません。
同じ要求の再送には409 `secret_not_replayable` と作成済みIDを返し、認証済みのrotate操作で回復します。
Idempotency-Keyの有効期間は24時間で、期限後の永続的な重複防止は保証しません。

## 通知と復帰

```sh
export MULTI_USER_AI_CONTEXT_MANAGER_TOKEN='<session-editor-token>'
bin/multi-user-ai-context-manager api --path "/v1/projects/$MULTI_USER_AI_CONTEXT_MANAGER_PROJECT/snapshot"
bin/multi-user-ai-context-manager watch --project "$MULTI_USER_AI_CONTEXT_MANAGER_PROJECT" --cursor-file ./session.cursor
```

watchは初回snapshotを出力し、そのカーソル以降を購読します。
イベント出力に成功した後にカーソルファイルを更新し、切断時は再接続します。
45秒間受信がない接続も張り直し、一時的なHTTP障害や429ではRetry-Afterに従って再接続します。
epoch変更や保持期限切れではsnapshotを取り直します。
出力先の利用者まで処理を終えたという保証はないため、外部処理を組み込むクライアントは、自分の永続状態と処理済みrevisionを併せて管理します。

イベントはプロジェクトrevision順で、少なくとも一回の配信です。
本文や秘密値は配信せず、種類、対象ID、版、主体、時刻を返します。
MVPは1秒ポーリング方式を採用し、各プロセスでプロジェクトごとに差分取得を共有します。
購読ごとのDB接続を保持せず、送信待ちを100件に制限し、遅い接続は閉じます。
書込み時にNOTIFYも発行しますが、専用LISTEN接続はこの版では利用しません。
SSEは15秒heartbeat、最大5分で再接続し、失効は5秒間隔で再確認します。
イベント保持は7日または100万件で、起動後は毎時pruneします。
手動では `bin/multi-user-ai-context-manager prune` を実行できます。

## 検証

```sh
go test ./...
go vet ./...
# TEST_DATABASE_URL はテスト用schemaと一時DBを作成できる専用PostgreSQLを指定
TEST_DATABASE_URL='postgres://user:password@127.0.0.1:5432/testdb?sslmode=disable' \
TEST_PG_BIN='/path/to/postgresql/bin' \
  go test -race -count=1 -v ./...
```

PostgreSQL 17と同版のpg_dump/pg_restoreを含む、隔離したCompose試験も実行できます。
テスト用DBはホストへ公開せず、アプリケーション用DBとは別の一時コンテナを使います。

```sh
docker compose -f compose.test.yaml up --build --abort-on-container-exit --exit-code-from tests
docker compose -f compose.test.yaml down
```

DBが未指定の場合、Go単体のDB統合試験はskipします。
`TEST_PG_BIN` を省略すると論理バックアップ復元試験だけをskipします。
復元試験には `pg_dump` と `pg_restore`、一時DBの作成権限が必要です。
受入結果と未検証事項は [docs/validation.md](docs/validation.md) に記録しています。

## GitHub Actions CI

[CIワークフロー](.github/workflows/ci.yml) はmainへのpush、pull request、手動実行で動きます。
GitHub-hosted Ubuntu上で、次の二つを独立したジョブとして実行します。

- PostgreSQL 17と同版のpg_dump/pg_restoreを含むCompose統合テスト（race detector付き）、gofmt、go vet。
- 通常のDockerfileによるアプリケーションイメージのビルド。

試験には隔離した一時DBとテスト専用設定を使い、ローカルの `.env` やトークンは渡しません。
Actionsの権限はリポジトリ内容の読取りだけにし、生成イメージの公開やデプロイは行いません。

## 配置と運用

設定は `DATABASE_URL` と `LISTEN_ADDR` です。
`LISTEN_ADDR` を省略するとlocalhost:8080、クラウドで `PORT` がある場合は0.0.0.0のそのポートで待ち受けます。
正本はDBだけに保存し、アプリケーションのローカルディスクを永続化する必要はありません。
TLS、バックアップ、復元、クラウド設定は [docs/operations.md](docs/operations.md) を参照してください。

共有本文は、投稿者のロールにかかわらずデータとして扱います。
URLの自動取得、HTMLの実行、外部ベンチ投入、デプロイ、エージェント起動はサービス自身では行いません。
leaseは協調する利用者間の調停であり、外部サービスで動いている操作を停止する保証はありません。

ライセンスは [Apache License 2.0](LICENSE)（SPDX: `Apache-2.0`）です。
依存ライブラリには、それぞれのライセンスが適用されます。
