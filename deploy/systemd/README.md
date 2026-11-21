# systemd 服务部署指南

本目录提供用于 Linux 物理机或虚拟机的 systemd 服务单元与环境配置模板，遵循最小权限沙箱、显式文件描述符上限和有序优雅关停设计。

---

## 1. 安装二进制与创建专用系统用户

1. 安装 `lazy-observe` 二进制到 `/usr/local/bin/lazy-observe`（默认来自 GitHub Release 归档或 `mise run dist` 产物）：

   ```sh
   curl -fsSL https://raw.githubusercontent.com/eimsound/lazy-observe/main/scripts/install.sh | sh
   lazy-observe --version
   ```

2. 创建无登录权限的专用系统账号与目录：

   ```sh
   sudo useradd --system --home-dir /var/lib/lazy-observe --shell /usr/sbin/nologin lazy-observe
   sudo install -d -m 0750 -o root -g lazy-observe /etc/lazy-observe
   sudo install -d -m 0750 -o lazy-observe -g lazy-observe /var/lib/lazy-observe
   ```

   > **关于 `User=lazy-observe` vs `DynamicUser=yes`**：本单元默认使用固定系统用户 `lazy-observe`，确保 `/var/lib/lazy-observe`（SQLite、本地 Blob、嵌入式存储数据）与 `/etc/lazy-observe/secret.key` 在重启、冷备和离线执行 `lazy-observe admin` / `lazy-observe doctor` 时拥有稳定属主。如仅做临时单机测试，可将 `User`/`Group` 替换为 `DynamicUser=yes` 并交由 systemd 管理 `StateDirectory`。

---

## 2. 准备配置文件与环境变量

```sh
# 1. 复制主配置文件并调整 data_dir 与 listen
sudo install -m 0640 -o root -g lazy-observe config.example.yaml /etc/lazy-observe/config.yaml
sudo sed -i 's|data_dir: "./data"|data_dir: "/var/lib/lazy-observe"|' /etc/lazy-observe/config.yaml

# 2. 复制环境变量覆写文件（存放敏感连接串时务必保持 0640 root:lazy-observe）
sudo install -m 0640 -o root -g lazy-observe \
  deploy/systemd/lazy-observe.env.example /etc/lazy-observe/lazy-observe.env
```

如部署为 Standalone / HA 节点，建议显式准备平台加密密钥文件与初始管理员密码文件（权限 `0600`，属主 `lazy-observe:lazy-observe`），并在启动服务前以同用户执行只读预检：

```sh
sudo -u lazy-observe env $(grep -v '^#' /etc/lazy-observe/lazy-observe.env | xargs) \
  /usr/local/bin/lazy-observe doctor --ha --config /etc/lazy-observe/config.yaml
```

---

## 3. 安装并启动 systemd 单元

```sh
sudo install -m 0644 deploy/systemd/lazy-observe.service /etc/systemd/system/lazy-observe.service
sudo systemctl daemon-reload
sudo systemctl enable --now lazy-observe
sudo systemctl status lazy-observe
```

查看日志与验证探针：

```sh
sudo journalctl -u lazy-observe -f
curl -fsS http://127.0.0.1:3100/livez
curl -fsS http://127.0.0.1:3100/readyz
curl -fsS http://127.0.0.1:3100/healthz
```

首次启动且未设置 `security.admin_password_file` 时，管理员随机初始密码位于 `/var/lib/lazy-observe/initial-admin-password`（权限 `0600`，属主 `lazy-observe`），首次登录修改密码后由网关自动删除：

```sh
sudo cat /var/lib/lazy-observe/initial-admin-password
```

---

## 4. 关键加固与运维要点

- **`ProtectSystem=strict` + `ReadWritePaths=/var/lib/lazy-observe`**：除数据目录和隔离的 `/tmp`（`PrivateTmp=true`）外，整个文件系统对服务只读。
  - 若将 `meta.db_url` 的 SQLite 文件或外部文件指向其他路径，请通过 `systemctl edit lazy-observe` 追加 `ReadWritePaths=`。
  - **切勿对 `/var/lib/lazy-observe` 挂载 `noexec`**：`lazy-observe` 启动时会校验并解压内嵌的 Victoria 二进制到 `/var/lib/lazy-observe/bin` 执行。
- **零特权端口绑定**：单元清除了 `CapabilityBoundingSet` 与 `AmbientCapabilities`。建议让网关监听 `127.0.0.1:3100`（或 `:3100`），由前置 Nginx / Caddy 终止 HTTPS，并设置 `security.session_secure_cookie: true` 与 `security.trusted_proxies`。若需直接监听 443 端口启用原生 TLS（`security.tls_cert` / `security.tls_key`），请通过 drop-in 追加：
  ```ini
  [Service]
  CapabilityBoundingSet=CAP_NET_BIND_SERVICE
  AmbientCapabilities=CAP_NET_BIND_SERVICE
  ```
- **优雅摘流与有序关停**：
  - 单元配置了 `KillMode=mixed`、`KillSignal=SIGTERM`、`TimeoutStopSec=45s`：systemd 先只向主进程发 `SIGTERM`，由内置 Supervisor 按「网关 drain（20s）→ 停 `vmagent` → 停嵌入存储（15s）」顺序优雅退出，超时后再清理 cgroup 残留子进程。
  - 若前方挂载了负载均衡，可在维护重启前先发送 `SIGUSR1` 让 `/readyz` 立刻返回 `503`，等待摘流完成后再重启：
    ```sh
    sudo systemctl kill -s SIGUSR1 --kill-who=main lazy-observe
    sleep 5
    sudo systemctl restart lazy-observe
    ```
- **备份、升级、回滚与应急重置**：完整手册见 [docs/operations.md](../../docs/operations.md)。离线运行 `admin` 子命令时请以 `lazy-observe` 用户执行，避免以 `root` 创建出服务无权读取的文件：
  ```sh
  sudo -u lazy-observe /usr/local/bin/lazy-observe admin reset-password \
    --config /etc/lazy-observe/config.yaml
  ```
