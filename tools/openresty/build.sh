#!/bin/sh
# 构建可在任意 Linux 发行版运行的 OpenResty（Web 转发的「二进制安装」使用）。
# 在 Alpine（musl）中编译，OpenSSL / PCRE2 / zlib 静态链接；程序自带 musl 动态加载器与依赖库
# （安装目录固定为 /usr/local/pve-forward-openresty），不依赖目标系统的 glibc 或软件包。
# 不做完全静态链接：OpenResty 的 resty.core 通过 LuaJIT FFI 查找程序内的符号，需要动态加载器。
#
#   docker run --rm -v "$PWD/tools/openresty:/w" -v "$PWD/dist:/out" alpine:3.22 sh /w/build.sh
#
# 产物：/out/openresty-<版本>-linux-<amd64|arm64>.tar.gz 及 .sha256
set -eu

. "$(dirname "$0")/versions.env"
PREFIX=/usr/local/pve-forward-openresty
case "$(uname -m)" in
  x86_64) ARCH=amd64; LDSO=ld-musl-x86_64.so.1 ;;
  aarch64) ARCH=arm64; LDSO=ld-musl-aarch64.so.1 ;;
  *) echo "不支持的架构 $(uname -m)" >&2; exit 1 ;;
esac

apk add --no-cache build-base perl linux-headers curl patchelf >/dev/null

SRC=$(mktemp -d)
cd "$SRC"
fetch() { # 下载并校验源码包：$1 地址，$2 SHA256
  f=$(basename "$1")
  curl -fsSL --retry 3 -o "$f" "$1"
  echo "$2  $f" | sha256sum -c - >/dev/null || { echo "校验失败：$f" >&2; exit 1; }
  tar xzf "$f"
}
fetch "https://openresty.org/download/openresty-$OPENRESTY_VERSION.tar.gz" "$OPENRESTY_SHA256"
fetch "https://github.com/openssl/openssl/releases/download/openssl-$OPENSSL_VERSION/openssl-$OPENSSL_VERSION.tar.gz" "$OPENSSL_SHA256"
fetch "https://github.com/PCRE2Project/pcre2/releases/download/pcre2-$PCRE2_VERSION/pcre2-$PCRE2_VERSION.tar.gz" "$PCRE2_SHA256"
fetch "https://github.com/madler/zlib/releases/download/v$ZLIB_VERSION/zlib-$ZLIB_VERSION.tar.gz" "$ZLIB_SHA256"

cd "openresty-$OPENRESTY_VERSION"
./configure --prefix="$PREFIX" \
  --with-cc-opt="-O2" \
  --with-openssl="$SRC/openssl-$OPENSSL_VERSION" --with-openssl-opt="no-shared no-tests no-docs" \
  --with-pcre="$SRC/pcre2-$PCRE2_VERSION" --with-pcre-jit \
  --with-zlib="$SRC/zlib-$ZLIB_VERSION" \
  --with-threads --with-file-aio \
  --with-http_ssl_module --with-http_v2_module --with-http_realip_module \
  --with-http_stub_status_module --with-http_gzip_static_module --with-http_sub_module \
  --with-stream --with-stream_ssl_module --with-stream_ssl_preread_module \
  -j"$(nproc)" >/dev/null
make -j"$(nproc)" >/dev/null
STAGE=$(mktemp -d)
make install DESTDIR="$STAGE" >/dev/null

R="$STAGE$PREFIX"
BIN="$R/nginx/sbin/nginx"
# 去掉用不到的工具与文档
rm -rf "$R/bin/opm" "$R/bin/resty" "$R/bin/restydoc" "$R/bin/restydoc-index" "$R/bin/md2pod.pl" "$R/bin/nginx-xml2pod" "$R/pod" "$R/site" "$R/luajit/share/man"
# 自带 musl 加载器与依赖库：设置程序的解释器与库搜索路径
mkdir -p "$R/lib"
cp "/lib/$LDSO" "$R/lib/"
# 程序与自带的 LuaJIT / Lua 模块依赖的系统库（如 libluajit 依赖 libgcc_s）都要带上
for f in "$BIN" "$R"/luajit/lib/*.so* "$R"/lualib/*.so; do
  [ -f "$f" ] || continue
  for so in $(LD_LIBRARY_PATH="$R/luajit/lib" ldd "$f" 2>/dev/null | awk '/=>/ { print $3 }'); do
    case "$so" in "$R"*|*/"$LDSO") ;; *) [ -f "$R/lib/$(basename "$so")" ] || cp -L "$so" "$R/lib/" ;; esac
  done
done
ls "$R/lib"
for f in "$BIN" "$R"/luajit/bin/luajit-*; do
  [ -f "$f" ] || continue
  patchelf --set-interpreter "$PREFIX/lib/$LDSO" --set-rpath "$PREFIX/lib:$PREFIX/luajit/lib" "$f"
done
for so in "$R"/lib/*.so* "$R"/luajit/lib/*.so* "$R"/lualib/*.so; do
  [ -f "$so" ] && [ "$(basename "$so")" != "$LDSO" ] && patchelf --set-rpath "$PREFIX/lib:$PREFIX/luajit/lib" "$so" 2>/dev/null || true
done
strip "$BIN" 2>/dev/null || true
echo "$OPENRESTY_VERSION-$BUNDLE_REVISION" > "$R/PVE_FORWARD_BUNDLE"

# 自检：装到实际路径，用自带的加载器运行，并加载 resty.core（需要 LuaJIT FFI 查找程序内符号）
rm -rf "$PREFIX" && cp -a "$R" "$PREFIX"
"$PREFIX/bin/openresty" -v
T=$(mktemp -d); mkdir -p "$T/logs"
printf 'events {}
http { lua_shared_dict t 1m; init_by_lua_block { require "resty.core" } server { listen 127.0.0.1:18080; location / { content_by_lua_block { ngx.shared.t:incr("k", 1, 0) ngx.say("ok ", ngx.shared.t:get("k")) } } } }
' > "$T/nginx.conf"
"$PREFIX/bin/openresty" -p "$T" -c "$T/nginx.conf"
sleep 1
[ "$(wget -qO- http://127.0.0.1:18080/)" = "ok 1" ] || { echo "Lua 自检失败" >&2; cat "$T/logs/error.log" >&2; exit 1; }
"$PREFIX/bin/openresty" -p "$T" -c "$T/nginx.conf" -s stop
mkdir -p /out
NAME="openresty-$OPENRESTY_VERSION-$BUNDLE_REVISION-linux-$ARCH.tar.gz"
tar -czf "/out/$NAME" -C "$STAGE/usr/local" pve-forward-openresty
(cd /out && sha256sum "$NAME" > "$NAME.sha256")
echo "已生成 /out/$NAME"
