# UtauTTSVoicevoxProxy
UtauTTSとVoicevoxの間で互換を生むための中継サーバー

ymm4のリクエストと互換を持たせている　が、\
アクセントフレーズのAPIはUtauTTSにないため\
入力を失わないためのプレースホルダーにしてる部分がある。

コードはAIちゃんに書かせた

utauTTS v1.5.4 とかに対応\
今後仕様が大きく変更されるかも

windows11 あとx86-64のみ

防備録

## build

go がインストールされた環境下で powershell

`
go build -o utautts-voicevox-proxy.exe .
`

## execute

当たり前だがutauttsのサーバーを起動すること

起動
`
.\utautts-voicevox-proxy.exe
`

話者一覧取得
```
curl.exe http://127.0.0.1:50021/speakers
```

音声合成にテスト
```
# 音声合成用のjson取得
curl.exe -X POST --get --data-urlencode "text=ほげぴゃ" "http://127.0.0.1:50021/audio_query" -o query.json

# jsonをutauTTSに送信、wavを取得する
curl.exe -X POST -H "Content-Type: application/json" --data-binary "@query.json" "http://127.0.0.1:50021/synthesis?speaker=0"
-o test.wav
```
