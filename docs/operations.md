# 配置と復元

## セルフホスト

起動コマンドはREADMEを参照してください。
`docker compose down` はコンテナを停止し、DBのvolumeを残します。
`down -v` はDBも削除するため、通常の停止には使いません。

外部公開時にはTLSを終端するreverse proxyを置きます。
[deploy/nginx.conf.example](../deploy/nginx.conf.example) はSSE bufferingを無効にする設定例です。
Authorization、トークン、要求本文をアクセスログへ追加しないでください。
DB側もSQLパラメーターのログ出力を無効にし、バックアップのアクセス権を制限します。

アプリケーションの標準poolは12接続です。
SSEはプロジェクト単位の共有ポーリングと通常のpoolを使い、購読ごとに接続を保持しません。
transaction poolingでも動作し、`DIRECT_DATABASE_URL` はこの版では使用しません。
`PUBLIC_BASE_URL` に依存する絶対URL生成もなく、API内の参照先は相対パスです。

DBスキーマは版1です。
`migrate` は管理用advisory lockで直列化し、`serve` は未適用や非互換のスキーマで起動しません。
公開前にmigrateを一回実行し、失敗時は新しいバイナリでserveを開始しない運用とします。

## クラウド用テンプレート

[.railway/railway.ts](../.railway/railway.ts) にAPIとPostgreSQLの設定を用意しています。
設定は現行の [Railway IaC](https://docs.railway.com/infrastructure-as-code) と [リファレンス](https://docs.railway.com/infrastructure-as-code/reference) に基づき、pre-deployでmigrate、開始時にserve、readinessで `/readyz` を使います。
旧Config as Codeは新規サービスに使えないため、`railway.toml` は配布しません。

このテンプレートはローカルで準備した設定で、クラウドへ適用していません。
利用開始時は独立した新規プロジェクトを用意し、費用とバックアップ設定を決定した後に配置します。
既存プロジェクトへ適用すると、設定にないリソースが削除候補になる場合があるため、新規の対象を選びます。

1. Railway CLIとIaC用の `railway` npmパッケージを、利用時の公式手順に従って導入する。
2. 新規プロジェクトを作成し、このディレクトリをlinkする。
3. `railway config plan` でAPIとDBの構成を確認する。
4. 内容と費用を確認した後にapplyし、ローカルソースをAPIサービスへアップロードするか、利用者が選択した非公開ソースを接続する。
5. Dockerfileでビルドし、migrateの成功とreadinessを確認する。
6. 管理用の非公開端末からbootstrapを実行し、プロジェクトと参加者を作る。
7. 公開URLからSSEのsnapshot後の差分、切断再接続、失効後の終了を試験する。

bootstrapをpre-deployや起動コマンドへ追加しないでください。
管理者の秘密値を公開ログへ出してしまうためです。
Railway固有のSDKはサーバーの実行時依存に含めていません。
長時間HTTPとPostgreSQLを提供する別のコンテナホストでも、同じイメージを使えます。

## バックアップ

DBには本文履歴、トークンのハッシュ、外部ジョブID、イベント、監査記録が含まれます。
バックアップにも機密情報が含まれるため、暗号化とアクセス制御を施し、別ディスクや別の保存先へ日次で保存します。
初期の保持期間は14日を目安に、チームで決定してください。

Composeでは以下のように論理バックアップを取れます。

```sh
umask 077
mkdir -p work/backups
docker compose exec -T db pg_dump -U agent_context -d agent_context -Fc \
  > work/backups/agent-context.dump
docker compose exec -T app /agent-context backup-manifest \
  > work/backups/manifest.json
```

`backup-manifest` は確認用のepochとスキーマ版を出力する補助です。
pg_dumpと同じsnapshotを保証するものではなく、復元epochの更新を代替しません。

## DB復元

復元はAPIへの流入を止めた状態で行います。
以下は既存DBを破壊せず、新規の空DB `agent_context_restored` に復元する例です。
接続先の切替えとバックアップの選択は運用者が行います。

```sh
docker compose stop app
docker compose exec -T db createdb -U agent_context agent_context_restored
docker compose exec -T db pg_restore -U agent_context -d agent_context_restored \
  --exit-on-error --no-owner --no-acl < work/backups/agent-context.dump
# .env のパスワードを使った新DB接続URLを RESTORED_DATABASE_URL に安全に設定する
docker compose run --rm -e DATABASE_URL="$RESTORED_DATABASE_URL" app \
  restore-epoch --reason 'restored from selected logical backup'
```

`restore-epoch` は世代IDを更新し、所有者のいたleaseを全て期限切れにし、世代番号を進め、lease hashと冪等応答を消去します。
所有者と外部ジョブの情報を残すため、再claimには復旧確認が必要です。
この管理操作は監査記録にも残ります。
同じDBへ複数アプリケーションが接続している場合は、全ての流入を停止してから実行します。

再開する前に、次を確認します。

1. バックアップ後に失効したトークンは古いDB状態から復活し得るため、必要な失効と管理者復旧を再適用する。
2. バックアップ後にredactした記録は再び本文を含み得るため、別保管した削除記録に基づきredactを再適用する。
3. checkpointに残る外部ジョブを確認し、走行中、終了済み、状態不明を区別する。
4. アプリケーションの `DATABASE_URL` を新DBへ切り替える。
5. readinessとsnapshotを確認し、旧カーソルが409、旧leaseが拒否されることを確かめる。
6. APIへの流入を再開する。

redactには `POST /contexts/:id/redact`、project_admin、理由、対象のIf-Matchが必要です。
本文と履歴の全版を置換し、現在版を進めてarchiveします。
旧If-Matchと通常のPATCHによる再書込みを拒否し、監査記録は対象IDと理由だけを残します。
理由へ機密本文を転載しないでください。
既存のバックアップからその場で本文を除去する機能はありません。

## 管理者の紛失

DBアクセス権を持つ運用者が、明示的に復旧します。

```sh
bin/agent-context admin recover --reason 'administrator token lost'
```

旧インスタンス管理者トークンを全て失効させ、新しいトークンを一度だけ表示します。
既存プロジェクトのproject_adminを全て紛失した場合も、DB運用者が復旧できます。
この操作は対象プロジェクトの旧project_adminトークンを全て失効させ、新しい主体とトークンを発行し、監査記録と変更イベントを保存します。

```sh
bin/agent-context admin project-recover --project '<project-id>' \
  --name 'recovered administrator' --reason 'project administrator tokens lost'
```
