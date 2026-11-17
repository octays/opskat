.PHONY: dev dev-sandbox dev-sandbox-down dev-sandbox-status run build build-embed install-app clean install build-cli install-cli build-ext lint test test-cover test-e2e test-e2e-scratch install-skill package package-clean package-checksums release-publish

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
    BIN_PATH := ./build/bin/opskat.app/Contents/MacOS/opskat
else ifeq ($(UNAME_S),Linux)
    BIN_PATH := ./build/bin/opskat
else
    BIN_PATH := ./build/bin/opskat.exe
endif

VERSION ?= 1.16.2
APP_INSTALL_DIR ?= $(HOME)/Applications
COMMIT_ID := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
VERSION_PKG := github.com/cago-frame/cago/configs
BUILDINFO_PKG := github.com/octays/opskat/internal/buildinfo
LDFLAGS := -s -w -X $(VERSION_PKG).Version=$(VERSION) -X $(BUILDINFO_PKG).CommitID=$(COMMIT_ID)

# 开发模式（前后端热重载）。用你自己的真实数据目录，供人手动开发/试用。
# 要做功能验证（尤其是会写数据的）请用 dev-sandbox，别在真实库上验。
dev:
	@mkdir -p frontend/dist && [ -e frontend/dist/.keep ] || touch frontend/dist/.keep
	wails3 dev

# 验证沙箱：在隔离数据目录上把真实应用跑起来并保持后台运行，附带一个无头 Chromium。
# 启动后用 e2e/drive.mjs 操作、e2e/oracle.mjs 读取副作用——一次性验证不再需要写 spec。
# 端口/数据目录按 checkout 分配，多个 worktree 可同时验证。流程见 docs/VERIFICATION.md。
# ARGS=--reset 清空沙箱数据，ARGS=--mocks 顺带起协议 mock，ARGS=--extensions 装上仓内扩展并开启扩展系统，ARGS=--headed 显示浏览器。
dev-sandbox:
	bun e2e/sandbox.mjs up $(ARGS)

dev-sandbox-down:
	bun e2e/sandbox.mjs down $(ARGS)

dev-sandbox-status:
	bun e2e/sandbox.mjs status

# 直接运行（不热重载）
run: build-embed
	$(BIN_PATH)

# 构建生产版本
build:
	bun scripts/desktop.mjs build --ldflags "$(LDFLAGS)"

# 构建生产版本（内嵌 opsctl CLI）
build-embed: build-cli-embed
	bun scripts/desktop.mjs build --ldflags "$(LDFLAGS)" --tags embed_opsctl

# 构建并安装 macOS 桌面应用（默认安装到当前用户的 ~/Applications）
ifeq ($(UNAME_S),Darwin)
install-app: build-embed
	@mkdir -p "$(APP_INSTALL_DIR)"
	@rm -rf "$(APP_INSTALL_DIR)/opskat.app"
	@ditto "./build/bin/opskat.app" "$(APP_INSTALL_DIR)/opskat.app"
	@echo "OpsKat installed to $(APP_INSTALL_DIR)/opskat.app"
else
install-app:
	@echo "install-app currently supports macOS only" >&2
	@exit 1
endif

# 构建 opsctl 用于嵌入桌面端
build-cli-embed:
	go build -ldflags="$(LDFLAGS)" -o ./internal/embedded/opsctl_bin ./cmd/opsctl/

# 安装前端依赖
install:
	cd frontend && bun x --bun pnpm@10.11.0 install

# 构建 opsctl CLI
build-cli:
	go build -ldflags="$(LDFLAGS)" -o ./build/bin/opsctl ./cmd/opsctl/

# 安装 opsctl 到 GOPATH/bin
install-cli:
	go install -ldflags="$(LDFLAGS)" ./cmd/opsctl/

# 构建仓内示例扩展：WASI reactor（go build -buildmode=c-shared），产物连同 manifest /
# SKILL.md / locales 一起落到 extensions/$(EXT)/dist，那正是应用要装的目录形状。
# 装进正在运行的应用（沙箱见 docs/VERIFICATION.md）：
#   make build-ext && opsctl ext dev $(CURDIR)/extensions/$(EXT)/dist
# 重跑这两条即热重载——暂存后原子替换，失败时保留旧版本；需桌面审批。
EXT ?= notebook
build-ext:
	@rm -rf extensions/$(EXT)/dist
	@mkdir -p extensions/$(EXT)/dist
	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o extensions/$(EXT)/dist/main.wasm ./extensions/$(EXT)
	@cp extensions/$(EXT)/manifest.json extensions/$(EXT)/dist/
	@[ -f extensions/$(EXT)/SKILL.md ] && cp extensions/$(EXT)/SKILL.md extensions/$(EXT)/dist/ || true
	@cp -R extensions/$(EXT)/locales extensions/$(EXT)/dist/
	@[ -d extensions/$(EXT)/frontend ] && cp -R extensions/$(EXT)/frontend/. extensions/$(EXT)/dist/ || true
	@echo "Built extensions/$(EXT)/dist — install it with: opsctl ext dev $(CURDIR)/extensions/$(EXT)/dist"

# 代码检查
lint:
	golangci-lint run --timeout 10m

# 代码检查并自动修复
lint-fix:
	golangci-lint run --timeout 10m --fix

# 运行测试
test:
	go test ./internal/... ./cmd/opsctl/... ./pkg/...

# E2E：Playwright 驱动真实 wails3 dev 跑 GUI 端到端。详见 docs/references/e2e-harness-guide.md。
# 一次性装依赖 + 浏览器：cd e2e && bun --bun run setup（CI 在独立步骤里装，故这里不重复）。
# 配方只做 shell 无关的 cd && pnpm，跨平台(cmd/sh 皆可)；编排与收尾清理(回收残留
# vite、删临时目录)都在 e2e/run-e2e.mjs 里用 Node 跨平台完成。
test-e2e:
	cd e2e && bun --bun run test

# 临时功能验证：跑 e2e/scratch/ 里的一次性 spec（不提交）。约定/用法见 docs/references/e2e-harness-guide.md。
test-e2e-scratch:
	cd e2e && bun --bun run test:scratch

# 测试覆盖率（生成 HTML 报告并在浏览器打开）
test-cover:
	go test -coverprofile=coverage.out ./internal/... ./cmd/opsctl/... ./pkg/...
	go tool cover -html=coverage.out -o coverage.html
	@echo "覆盖率报告已生成: coverage.html"
	@open coverage.html 2>/dev/null || xdg-open coverage.html 2>/dev/null || echo "请手动打开 coverage.html"

# 安装 Claude Code plugin（创建 symlink，注册 marketplace + plugin）
install-skill:
	@# 清理旧路径
	@rm -rf ~/.claude/skills/opsctl ~/.claude/plugins/cache/opskat
	@# Marketplace symlink → 市场根目录（含 .claude-plugin/marketplace.json + opsctl/ 插件目录）
	@rm -rf ~/.claude/plugins/marketplaces/opskat
	@mkdir -p ~/.claude/plugins/marketplaces
	@ln -s $(CURDIR)/plugin ~/.claude/plugins/marketplaces/opskat
	@# 注册到 installed_plugins.json + known_marketplaces.json + settings.json
	@python3 -c "\
	import json, os, datetime; \
	home = os.path.expanduser('~'); \
	now = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.000Z'); \
	plugin_path = os.path.join(home, '.claude/plugins/marketplaces/opskat/opsctl'); \
	mkt_path = os.path.join(home, '.claude/plugins/marketplaces/opskat'); \
	key = 'opsctl@opskat'; \
	pf = os.path.join(home, '.claude/plugins/installed_plugins.json'); \
	cfg = json.load(open(pf)) if os.path.exists(pf) else {'version': 2, 'plugins': {}}; \
	entries = cfg['plugins'].get(key, []); \
	ue = [e for e in entries if e.get('scope') == 'user']; \
	(ue[0].update({'installPath': plugin_path, 'version': 'dev', 'lastUpdated': now}) if ue else \
	 entries.append({'scope': 'user', 'installPath': plugin_path, 'version': 'dev', 'installedAt': now, 'lastUpdated': now})); \
	cfg['plugins'].pop('opsctl@local', None); \
	cfg['plugins'][key] = entries; \
	json.dump(cfg, open(pf, 'w'), indent=2, ensure_ascii=False); \
	kf = os.path.join(home, '.claude/plugins/known_marketplaces.json'); \
	km = json.load(open(kf)) if os.path.exists(kf) else {}; \
	km['opskat'] = {'source': {'source': 'directory', 'path': mkt_path}, 'installLocation': mkt_path, 'lastUpdated': now}; \
	json.dump(km, open(kf, 'w'), indent=2, ensure_ascii=False); \
	sf = os.path.join(home, '.claude/settings.json'); \
	sc = json.load(open(sf)) if os.path.exists(sf) else {}; \
	sc.setdefault('enabledPlugins', {})[key] = True; \
	sc.setdefault('extraKnownMarketplaces', {})['opskat'] = {'source': {'source': 'directory', 'path': mkt_path}}; \
	json.dump(sc, open(sf, 'w'), indent=2, ensure_ascii=False); \
	print(f'Registered plugin: {key}')"
	@echo "Plugin installed: marketplace -> $(CURDIR)/plugin"

# 清理构建产物
clean:
	rm -rf build/bin build/release frontend/dist internal/embedded/opsctl_bin \
		extensions/*/dist \
		coverage.out coverage.html coverage_new.out \
		opskat opsctl \
		frontend/package.json.md5

# ============================ 本地跨平台打包与发布 ============================
# 与 CI（build-desktop.yml + release.yml）产出同构：opskat-v<VERSION>-<platform>.(dmg|deb|
# -installer.exe) + windows 便携 zip + SHA256SUMS.txt + release-info.json——自更新器
# (update_svc)按「资产名含 <goos>-<goarch>」匹配、镜像回退读 release-info.json，布局不能改。
# Actions 不可用（禁用/fork 不触发）时的本地出路：
#   make package                                   # 默认 darwin-arm64,darwin-amd64
#   make package PLATFORMS=windows-amd64           # 需 mingw-w64 + makensis
#   make package PLATFORMS=linux-amd64             # 需 docker（容器内装 gtk/webkit）
#   make release-publish                           # 打包 + gh 发布到 origin 仓库
# 工具链：darwin 原生交叉即可；windows: brew install mingw-w64 makensis；linux: docker。
FILE_VERSION ?= v$(VERSION)
PACKAGE_DIR ?= build/release
# ad-hoc 签名；正式发布置为 "Developer ID Application: ..."
SIGNING_IDENTITY ?= -
GH_REPO ?= $(shell git remote get-url origin 2>/dev/null | sed -E 's#.*[:/]([^/]+/[^/.]+)(\.git)?$$#\1#')
PLATFORMS ?= darwin-arm64 darwin-amd64

package: package-clean
	@for p in $(subst $(comma), ,$(PLATFORMS)); do \
		$(MAKE) --no-print-directory package-$$p || exit 1; \
	done
	$(MAKE) --no-print-directory package-checksums
	@echo "产物在 $(PACKAGE_DIR)/："; ls -lh $(PACKAGE_DIR)

comma := ,

package-clean:
	rm -rf $(PACKAGE_DIR) build/release-stage

package-checksums:
	cd $(PACKAGE_DIR) && shasum -a 256 * > SHA256SUMS.txt

# ----- macOS：dmg（.app + /Applications 快捷方式），hdiutil 仅 mac 有 -----
package-darwin-%:
	$(eval ARCH := $*)
	@command -v hdiutil >/dev/null || { echo "需要 macOS 的 hdiutil" >&2; exit 1; }
	GOOS=darwin GOARCH=$(ARCH) go build -ldflags="$(LDFLAGS)" -o ./internal/embedded/opsctl_bin ./cmd/opsctl/
	bun scripts/desktop.mjs build --platform darwin/$(ARCH) --ldflags "$(LDFLAGS)" --tags embed_opsctl
	codesign --force --deep --sign "$(SIGNING_IDENTITY)" --options runtime \
		--entitlements build/darwin/entitlements.plist build/bin/opskat.app
	mkdir -p $(PACKAGE_DIR)
	rm -rf build/release-stage && mkdir -p build/release-stage
	cp -R build/bin/opskat.app build/release-stage/
	ln -s /Applications build/release-stage/Applications
	hdiutil create -volname "Opskat" -srcfolder build/release-stage -ov -format UDZO \
		$(PACKAGE_DIR)/opskat-$(FILE_VERSION)-darwin-$(ARCH).dmg
	rm -rf build/release-stage

# ----- Windows：NSIS 安装包（+amd64 便携 zip）。交叉需 mingw 与 makensis -----
package-windows-%:
	$(eval ARCH := $*)
	$(eval CC := $(if $(filter amd64,$(ARCH)),x86_64,aarch64)-w64-mingw32-gcc)
	@command -v $(CC) >/dev/null || { echo "缺交叉编译器 $(CC)：brew install mingw-w64" >&2; exit 1; }
	@command -v makensis >/dev/null || { echo "缺 NSIS：brew install makensis" >&2; exit 1; }
	GOOS=windows GOARCH=$(ARCH) go build -ldflags="$(LDFLAGS)" -o ./internal/embedded/opsctl_bin ./cmd/opsctl/
	CC=$(CC) bun scripts/desktop.mjs build --nsis --platform windows/$(ARCH) --ldflags "$(LDFLAGS)" --tags embed_opsctl
	mkdir -p $(PACKAGE_DIR)
	mv build/bin/opskat-$(ARCH)-installer.exe $(PACKAGE_DIR)/opskat-$(FILE_VERSION)-windows-$(ARCH)-installer.exe
ifeq ($(ARCH),amd64)
	rm -rf $(PACKAGE_DIR)/portable && mkdir -p $(PACKAGE_DIR)/portable/opskat-$(FILE_VERSION)-windows-$(ARCH)/data
	cp build/bin/opskat.exe $(PACKAGE_DIR)/portable/opskat-$(FILE_VERSION)-windows-$(ARCH)/
	cp internal/embedded/opsctl_bin $(PACKAGE_DIR)/portable/opskat-$(FILE_VERSION)-windows-$(ARCH)/opsctl.exe
	cp build/windows/portable-data-readme.txt $(PACKAGE_DIR)/portable/opskat-$(FILE_VERSION)-windows-$(ARCH)/data/README.txt
	cd $(PACKAGE_DIR)/portable && zip -qr ../opskat-$(FILE_VERSION)-windows-$(ARCH)-portable.zip . && cd ../..
	rm -rf $(PACKAGE_DIR)/portable
endif

# ----- Linux：deb。gtk/webkit 的 cgo 依赖太重不做宿主交叉，进 golang 容器构建 -----
# node_modules 用具名卷隔离：容器装的 linux 二进制不落回宿主目录（否则 mac 前端环境被污染）。
# 跨架构（如 M 芯片打 amd64）由 Docker 走 Rosetta 模拟，明显变慢属预期。
package-linux-%:
	$(eval ARCH := $*)
	@command -v docker >/dev/null || { echo "linux 打包需要 docker" >&2; exit 1; }
	mkdir -p $(PACKAGE_DIR)
	docker run --rm --platform linux/$(ARCH) -v $(CURDIR):/src -w /src \
		-v opskat-pkg-node-modules:/src/frontend/node_modules \
		-v opskat-pkg-frontend-dist:/src/frontend/dist \
		-v opskat-pkg-go-cache:/go \
		-e FILE_VERSION=$(FILE_VERSION) -e VERSION=$(VERSION) -e COMMIT_ID=$(COMMIT_ID) -e TARGET_ARCH=$(ARCH) \
		golang:1.27 bash -euxo pipefail -c '\
			apt-get update -qq && apt-get install -y -qq --no-install-recommends libgtk-4-dev libwebkitgtk-6.0-dev zip unzip >/dev/null; \
			curl -fsSL https://bun.sh/install | BUN_INSTALL=/usr/local bash >/dev/null; \
			cd frontend && bun x --bun pnpm@10.11.0 install --frozen-lockfile && cd ..; \
			go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0 && export PATH="$$PATH:/go/bin"; \
			go build -ldflags="-s -w -X github.com/cago-frame/cago/configs.Version=$$VERSION -X github.com/octays/opskat/internal/buildinfo.CommitID=$$COMMIT_ID" -o ./internal/embedded/opsctl_bin ./cmd/opsctl/; \
			bun scripts/desktop.mjs build --platform linux/$$TARGET_ARCH --ldflags "-s -w -X github.com/cago-frame/cago/configs.Version=$$VERSION -X github.com/octays/opskat/internal/buildinfo.CommitID=$$COMMIT_ID" --tags embed_opsctl; \
			rm -rf /tmp/deb && mkdir -p /tmp/deb/DEBIAN /tmp/deb/usr/bin /tmp/deb/usr/share/applications /tmp/deb/usr/share/icons/hicolor/256x256/apps; \
			cp build/bin/opskat /tmp/deb/usr/bin/opskat; \
			cp build/appicon.png /tmp/deb/usr/share/icons/hicolor/256x256/apps/opskat.png; \
			DEB_VER=$$(echo "$$VERSION" | sed "s/^v//; s/-/~/g"); \
			printf "Package: opskat\nVersion: %s\nSection: utils\nPriority: optional\nArchitecture: %s\nMaintainer: opskat\nDescription: Opskat desktop application\n" "$$DEB_VER" "$$TARGET_ARCH" > /tmp/deb/DEBIAN/control; \
			printf "[Desktop Entry]\nName=Opskat\nExec=/usr/bin/opskat\nIcon=opskat\nType=Application\nCategories=Utility;\n" > /tmp/deb/usr/share/applications/opskat.desktop; \
			dpkg-deb --build /tmp/deb "opskat-$$FILE_VERSION-linux-$$TARGET_ARCH.deb" >/dev/null; \
			mv "opskat-$$FILE_VERSION-linux-$$TARGET_ARCH.deb" /src/$(PACKAGE_DIR)/'
	@echo "linux-$(ARCH) 打包完成"

# ----- 发布：产物 → GitHub Release（对齐 release.yml：删旧建新 + SHA256SUMS + release-info.json）-----
release-publish: package
	@command -v gh >/dev/null || { echo "需要 gh CLI 并已登录" >&2; exit 1; }
	$(eval TAG := v$(VERSION))
	$(eval TAG_FLAG := $(if $(findstring beta,$(VERSION)),--prerelease,$(if $(findstring rc,$(VERSION)),--prerelease,)))
	gh release delete $(TAG) -y 2>/dev/null || true
	gh release create $(TAG) --title $(TAG) --generate-notes $(TAG_FLAG) $(PACKAGE_DIR)/*
	gh api repos/$(GH_REPO)/releases/tags/$(TAG) > $(PACKAGE_DIR)/release-info.json
	gh release upload $(TAG) $(PACKAGE_DIR)/release-info.json --clobber
	@echo "已发布 $(GH_REPO) 的 $(TAG)"
