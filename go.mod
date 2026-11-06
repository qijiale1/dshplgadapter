module dshplgadapter

go 1.27.1

require github.com/JianFeeeee/hologramagentsdk v1.4.0

// SDK 指向本机源码。
//
// ★ 为何默认走 replace 而不是直接拉模块（2026-10-08 更新）：
//   SDK 已从 gitcode 迁到 GitHub（module 路径 github.com/JianFeeeee/hologramagentsdk）。
//   仓库虽在 GitHub，但本工程模板仍默认指向**本机源码**，原因与托管地无关：
//   （1）hmapdev 装的是本机 SDK（~/.hologram/hmapdev/sdk），离线也能构建；
//   （2）插件必须与内核的 SDK 版本严格一致，走网络拉会引入版本漂移；
//   （3）内网/受限出口环境拉不到公共 proxy。
//   若你已有可访问的私有 proxy 或愿意走公网，可删掉本行。





replace github.com/JianFeeeee/hologramagentsdk => /home/hatch/.hologram/hmapdev/sdk/v1.4.0
