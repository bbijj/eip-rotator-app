## EIP Rotator (UCloud)

使用 UCloud Go SDK 在指定项目下定时为绑定到 UHost 的 EIP 做“跳变”（新建相同规格 EIP，解绑旧 EIP，绑定新 EIP，并释放旧 EIP）。

### 安装

1) 进入目录并初始化依赖：

```
cd /Users/user/script/eip-rotator-app
go mod tidy
```

2) 构建：

```
go build -o bin/eip-rotator ./cmd/eip-rotator
```

### 直接执行一次

```
bin/eip-rotator \
  --mode run \
  --public-key $UCLOUD_PUBLIC_KEY \
  --private-key $UCLOUD_PRIVATE_KEY \
  --project-ids org-xxx,org-yyy

# 可选：若仅想处理某一地域，追加 --region 参数
# --region cn-bj2
```

或使用 JSON 任务配置：

```
bin/eip-rotator --mode run --config ./configs/tasks.example.json
```

### 定时执行（容器内置调度器，Ubuntu 22.04 Docker）

- 本项目内置秒级调度器，不依赖系统 cron/launchd。任务以配置文件热更新：
  - 相同“公钥+私钥+项目ID列表”视为同一任务，region/interval 变化会自动更新；
  - 新增键追加任务；从配置删除则停止任务。

#### 容器构建与运行

```
docker build -t eip-rotator:latest /Users/user/script/eip-rotator-app
docker run -d --name eip-rotator \
  -v $(pwd)/configs/tasks.example.json:/app/tasks.json:ro \
  eip-rotator:latest
```

或指定自定义配置路径：

```
docker run -d --name eip-rotator \
  -v /path/to/tasks.json:/app/tasks.json:ro \
  eip-rotator:latest
```

### 注意
- Region 可选：
  - 未指定 `region` 或为空时，工具会通过 UAccount.GetRegion 自动枚举当前账号可访问的所有地域并全量扫描。
  - 指定 `region` 时，仅在该地域执行。
- 需要确保账号与项目在对应地域已开通 EIP 资源配额，并具备创建/解绑/绑定/释放 EIP 的权限。
- 生产环境建议先在少量主机上试运行，确认业务可接受短暂 IP 切换影响。

