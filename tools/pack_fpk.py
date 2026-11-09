#!/usr/bin/env python3
"""把 fpk-x86 / fpk-arm 源目录打成 fnOS 可安装的 .fpk。

结构（与 2.4.x 已发布的包一致）：
    fpk  = gzip( tar( app.tgz, cmd/, config/, ICON*.PNG, manifest, wizard/ ) )
    app.tgz = gzip( tar( mediaview, ui/, config/ ) )
    manifest 里的 checksum = md5(app.tgz)

用法：
    python tools/pack_fpk.py fpk-x86 dist/mediaview_2.5.0_x86.fpk
    python tools/pack_fpk.py fpk-arm dist/mediaview_2.5.0_arm.fpk

设计约束：
  · app.tgz 必须**先**生成，因为 manifest 里的 checksum 依赖它；
  · 所有条目 mtime 固定为 0、uid/gid=0、uname/gname 置空 —— 同样的输入得到同样的字节，
    重新打包不会出现"内容没变但 fpk 变了"的假差异（之前排查版本问题时被这个坑过）；
  · 目录条目 mode 0777、文件 0666，与 fnOS 官方 fnpack 的产物一致。
"""
import gzip
import hashlib
import io
import os
import sys
import tarfile

MTIME = 0
DIR_MODE = 0o777
FILE_MODE = 0o666


def _add(tf, arcname, mode, data=None, isdir=False):
    ti = tarfile.TarInfo(arcname)
    ti.mode = mode
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = ''
    ti.mtime = MTIME
    if isdir:
        ti.type = tarfile.DIRTYPE
        ti.size = 0
        tf.addfile(ti)
    else:
        ti.size = len(data)
        tf.addfile(ti, io.BytesIO(data))


def _walk(tf, root, rel=''):
    """目录递归：目录条目先于其子项，子项按名字排序（可复现）。"""
    base = os.path.join(root, rel) if rel else root
    for name in sorted(os.listdir(base)):
        full = os.path.join(base, name)
        arc = (rel + '/' + name) if rel else name
        if os.path.isdir(full):
            _add(tf, arc, DIR_MODE, isdir=True)
            _walk(tf, root, arc)
        else:
            with open(full, 'rb') as f:
                _add(tf, arc, FILE_MODE, f.read())


def _tgz(build):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode='w', format=tarfile.GNU_FORMAT) as tf:
        build(tf)
    return gzip.compress(buf.getvalue(), compresslevel=9, mtime=0)


def main():
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    src, out = sys.argv[1], sys.argv[2]

    app_dir = os.path.join(src, 'app')
    binary = os.path.join(app_dir, 'mediaview')
    if not os.path.isfile(binary):
        print('缺少 %s（先把二进制编译进去）' % binary)
        return 1

    def build_app(tf):
        with open(binary, 'rb') as f:
            _add(tf, 'mediaview', FILE_MODE, f.read())
        # ui / config 原样带上（app 目录下除二进制外的内容）
        for name in sorted(os.listdir(app_dir)):
            if name == 'mediaview':
                continue
            full = os.path.join(app_dir, name)
            if os.path.isdir(full):
                _add(tf, name, DIR_MODE, isdir=True)
                _walk(tf, app_dir, name)
            else:
                with open(full, 'rb') as f:
                    _add(tf, name, FILE_MODE, f.read())

    app_tgz = _tgz(build_app)
    checksum = hashlib.md5(app_tgz).hexdigest()

    # manifest：原有行 + 覆盖 checksum（源目录里不保留 checksum，避免版本串味）
    man_path = os.path.join(src, 'manifest')
    lines = []
    with open(man_path, encoding='utf-8') as f:
        for line in f.read().splitlines():
            if line.split('=')[0].strip() == 'checksum':
                continue
            lines.append(line)
    while lines and not lines[-1].strip():
        lines.pop()
    lines.append('checksum              = %s' % checksum)
    manifest = ('\n'.join(lines) + '\n').encode('utf-8')

    def build_fpk(tf):
        _add(tf, 'app.tgz', FILE_MODE, app_tgz)
        for name in sorted(os.listdir(src)):
            if name == 'app':
                continue
            full = os.path.join(src, name)
            if os.path.isdir(full):
                _add(tf, name, DIR_MODE, isdir=True)
                _walk(tf, src, name)
            elif name == 'manifest':
                _add(tf, name, FILE_MODE, manifest)
            else:
                with open(full, 'rb') as f:
                    _add(tf, name, FILE_MODE, f.read())

    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, 'wb') as f:
        f.write(_tgz(build_fpk))

    print('built %s' % out)
    print('  size     %d' % os.path.getsize(out))
    print('  app.tgz  %d bytes, md5=%s' % (len(app_tgz), checksum))
    return 0


if __name__ == '__main__':
    sys.exit(main())
