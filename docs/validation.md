# ローカル受入結果

検証日：2026-11-15（Asia/Tokyo）。
初期検証はmacOS arm64、Go 1.27.3、専用PostgreSQL 12.22で行いました。
レビュー後はRancher DesktopのDocker 29.9.2上で、通常構成と試験用Composeを実際に起動しました。
試験用イメージはGo 1.25系とPostgreSQL 17.12で、pg_dumpとpg_restoreも17系です。

## 実行した検証

| 検証 | 結果 |
| --- | --- |
| `go test -race -count=1 -v ./...`（専用DBとpgツール指定） | 23件成功、skipなし、race検出なし（PostgreSQL 17のCompose上） |
| `go vet ./...` とgofmt確認 | 成功 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/multi-user-ai-context-manager` | 成功 |
| `docker compose config --quiet` | 成功 |
| 通常Dockerfileの実ビルドとCompose起動 | 成功。appとdbのhealthcheckを確認 |
| PostgreSQL 17.12、pg_dump/pg_restore 17による全統合試験 | 成功。隔離したcompose.test.yamlで実行 |
| SKILLの形式検証と別ディレクトリへのインストール | 成功。既存同名スキルを上書きしないことも確認 |
| インストール済みSKILLヘルパーから実サービスへの参加 | snapshot、証跡投稿、TODO作成、claim、証跡付きcompleteが成功 |
| OpenAPI 3.1スキーマと参照の検証 | 成功（swagger-parser） |
| RailwayテンプレートのTypeScript strict型検査（railway SDK 3.17.0） | 成功 |
| 実バイナリのmigrate、秘密ファイルへのbootstrap、serve、health | 成功 |
| CLIでproject create、token issue、認証付きsnapshot | 成功 |
| 埋込み管理画面のHTTP応答と本文を文字列で表示する実装の確認 | 成功。ブラウザでの目視試験は未実施 |

通常構成の再現手順は `./scripts/dev-up.sh`、隔離した全試験の再現手順は以下です。

```sh
docker compose -f compose.test.yaml up --build --abort-on-container-exit --exit-code-from tests
docker compose -f compose.test.yaml down
```

## 不変条件の試験

- 同じ版への同時更新は一つだけ成功し、もう一方は412になる。旧版を参照できる。
- イベント保存を意図的に失敗させると、本文、履歴、revisionの全てがrollbackする。
- 同一要求の再送は同じIDを返し、異なる要求の同一キー利用は409になる。秘密値の再送は409になる。
- viewerの更新とeditorのトークン発行を拒否し、トークンの期限切れと失効を拒否する。
- pinは本文の新版追加で移動せず、要約の参照元更新をstaleとして示す。日本語部分一致検索が動く。
- context-packは指定したJSON応答のバイト数を超えず、不変pinを採用版として返す。
- 依存サイクルの同時追加は一方を拒否する。ブロッカーがclaimを妨げ、証跡付き解消後にclaimできる。
- 同時claimの成功者は一人だけになる。旧所有者、誤った秘密値、古い世代の完了を拒否する。
- 期限切れleaseの再取得は復旧メモと外部状態の確認を要求する。説明編集による所有権検証の迂回を拒否する。
- lease rotateで世代番号を進め、旧秘密値を拒否する。外部ジョブIDはプロセス再起動後も保持される。
- snapshot後、購読開始前の更新と、切断中の更新を再送する。ライブSSEでも更新を取得する。
- SSEのafterとLast-Event-IDが異なる要求を拒否し、失効後の接続を5秒程度で閉じる。
- 100件を超えて送信待ちを増やさず、遅い購読を閉じる。これはキュー単位の試験で、低速ネットワークの負荷試験ではない。
- 別プロジェクトの本文取得、pin参照、TODO依存参照、イベントカーソルを拒否する。
- 古いイベントをpruneすると保持floorを進め、古いカーソルへ410を返す。
- トークンrotateは旧トークンを失効させ、同じ主体の新トークンを発行する。
- redactは本文の全版を置換する。管理者復旧は監査記録を残して旧管理者を失効させる。
- snapshotのタスク上限超過をtruncatedで示し、ページ取得で205件全てを回収できる。
- プロジェクト行ロックを待っている書込み要求は、待機中にトークンが失効すると、ロック取得後の再認証で拒否される。
- 論理バックアップを新規DBへ復元し、epoch更新後に旧カーソルと旧leaseを拒否し、外部ジョブIDを保持する。
- CLI watchは切断後に保存カーソルから再接続し、epoch変更時にはsnapshotを取り直す。

テストは専用の一時schemaと一時DBを作成し、終了後に削除します。
既存サービス、共有サーバー、ISUCONのリポジトリやログは変更していません。

レビューで追加した試験は、redact後の旧版拒否、採用版の省略表示、大きな整数の保持、watchの不正URLと保存先と一時障害、epoch変更直後のSSE再接続です。
修正内容は `review.md` を参照してください。

## 設計からの実装上の選択

通知の正しさは永続イベントの取得に依存させ、MVPでは設計にある1秒ポーリングモードを採用しました。
NOTIFYは発行しますが、LISTEN接続とDIRECT_DATABASE_URLは未使用です。
context-packは最大200件の候補と100件のpinから決定的に選び、省略を明示します。
画面は接続確認、snapshot、プロジェクト作成、トークン発行と一覧だけに絞りました。

## 残る制限

クラウドでのSSE受入試験は未実施です。
通常Composeはlocalhost:8080で起動した状態を残し、試験用Composeだけを終了しました。
共有チームでの試用と、10万版、1000未完了TODOでの性能目標は未測定です。
HTTP認証とTLSの本番境界は入口の設定に依存し、頻度制限はプロセス単位です。
ライセンスはApache License 2.0です。
GitHubの公開リポジトリ `bootjp/multi-user-ai-context-manager` で管理しています。
クラウドへの配備は行っていません。

## 継続的な検証

`.github/workflows/ci.yml` に、mainへのpushとpull requestで実行するCIを定義しています。
統合テストは同じ `compose.test.yaml` を使い、PostgreSQL 17の復元試験を省略せず実行します。
別ジョブでアプリケーション用Dockerfileをビルドします。
各実行の成否は、対象コミットのGitHub Actions結果を確認してください。

## 識別子統一とローカル移行

CLIとSKILLのフォルダー名、コマンド名、Composeのプロジェクト名、DBとロール、Goのmodule pathを新名称へ統一しました。
環境変数の標準名は `MULTI_USER_AI_CONTEXT_MANAGER_*` です。
既存接続ファイルを保持するため、旧 `AGENT_CONTEXT_*` からの読取り互換と新変数の優先順位を試験しています。

移行はpreviewを確認後に実施し、DBアーカイブの一覧を検証してから新volumeへ復元しました。
17テーブルを確認し、復元で破棄する冪等応答キャッシュを除く16テーブルの件数と内容のハッシュを照合しました。
この照合では復元epoch、leaseの世代と有効期限、移行で追加した監査記録だけを比較対象から除いています。
元の `.env` と接続ファイルはバイト列のハッシュ一致を確認し、新変数の接続ファイルは別名で生成しています。
旧コンテナと旧volumeは停止状態で残し、バックアップは非公開の `work/backups/` に保存しました。
