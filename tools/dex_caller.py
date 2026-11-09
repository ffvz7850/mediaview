#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""最小 DEX 解析器：找出「谁调用了某个方法」。

用途：飞牛 App 里 `clearHistory` / `canGoBack` / `goBack` 到底被谁调用 ——
决定 fpk 网页里的 history 守卫能不能活下来。

只做一件事：扫所有方法的字节码，匹配 invoke-* 引用的 method_idx，
打印「调用方类名#方法名 -> 被调类名#方法名」。不做完整反编译。
"""
import struct
import sys
import zipfile


def uleb128(b, i):
    result = 0
    shift = 0
    while True:
        byte = b[i]
        i += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, i
        shift += 7


class Dex:
    def __init__(self, data):
        self.d = data
        (self.string_ids_size, self.string_ids_off,
         self.type_ids_size, self.type_ids_off,
         self.proto_ids_size, self.proto_ids_off,
         self.field_ids_size, self.field_ids_off,
         self.method_ids_size, self.method_ids_off,
         self.class_defs_size, self.class_defs_off) = struct.unpack_from('<12I', data, 0x38)
        self._strings = {}

    def string(self, idx):
        if idx in self._strings:
            return self._strings[idx]
        off = struct.unpack_from('<I', self.d, self.string_ids_off + idx * 4)[0]
        n, i = uleb128(self.d, off)
        end = self.d.index(b'\x00', i)
        s = self.d[i:end].decode('utf-8', 'replace')
        self._strings[idx] = s
        return s

    def type_name(self, idx):
        return self.string(struct.unpack_from('<I', self.d, self.type_ids_off + idx * 4)[0])

    def method(self, idx):
        off = self.method_ids_off + idx * 8
        class_idx, proto_idx, name_idx = struct.unpack_from('<HHI', self.d, off)
        return self.type_name(class_idx), self.string(name_idx)

    def class_name(self, class_idx):
        return self.type_name(class_idx)

    def methods(self):
        """遍历所有 class_def 的 direct/virtual methods，产出 (类名, 方法名, code_off)。"""
        for ci in range(self.class_defs_size):
            base = self.class_defs_off + ci * 32
            class_idx = struct.unpack_from('<I', self.d, base)[0]
            class_data_off = struct.unpack_from('<I', self.d, base + 24)[0]
            cname = self.class_name(class_idx)
            if not class_data_off:
                continue
            i = class_data_off
            sf, i = uleb128(self.d, i)
            inf, i = uleb128(self.d, i)
            dm, i = uleb128(self.d, i)
            vm, i = uleb128(self.d, i)
            # 跳过字段
            for _ in range(sf + inf):
                _, i = uleb128(self.d, i)
                _, i = uleb128(self.d, i)
            for count in (dm, vm):
                midx = 0
                for _ in range(count):
                    diff, i = uleb128(self.d, i)
                    midx += diff
                    _flags, i = uleb128(self.d, i)
                    code_off, i = uleb128(self.d, i)
                    if code_off:
                        yield cname, midx, code_off

    def method_name(self, method_idx):
        return self.method(method_idx)[1]

    def code_insns(self, code_off):
        registers, ins, outs, tries = struct.unpack_from('<4H', self.d, code_off)
        insns_size = struct.unpack_from('<I', self.d, code_off + 12)[0]
        start = code_off + 16
        return struct.unpack_from('<%dH' % insns_size, self.d, start), insns_size

    def callees(self, code_off):
        """返回该方法里所有 invoke-* 的 (method_idx, 被调方法名, 被调类名)。"""
        try:
            insns, n = self.code_insns(code_off)
        except Exception:
            return
        i = 0
        while i < n:
            unit = insns[i]
            op = unit & 0xFF
            if 0x6E <= op <= 0x72 or 0x74 <= op <= 0x78:
                if i + 1 < n:
                    midx = insns[i + 1]
                    try:
                        cname, mname = self.method(midx)
                    except Exception:
                        cname, mname = '?', '?'
                    yield midx, mname, cname
                i += 3 if op <= 0x72 else 3
                continue
            # 其它指令长度：只用 35c/3rc 需要跳 3；这里保守地逐个走（够用，会多扫但不会漏 3 单元组）
            i += 1


def scan(apk, dex_names, targets):
    z = zipfile.ZipFile(apk)
    for dn in dex_names:
        data = z.read(dn)
        dx = Dex(data)
        print('=== %s  (%d strings, %d methods) ===' % (dn, dx.string_ids_size, dx.method_ids_size))

        # 先建立 method_idx -> name 的目标集合
        want = {}
        for mi in range(dx.method_ids_size):
            try:
                cname, mname = dx.method(mi)
            except Exception:
                continue
            if mname in targets:
                want[mi] = (cname, mname)

        found = 0
        for cls, midx, code_off in dx.methods():
            try:
                cname, mname = dx.method(midx)
            except Exception:
                continue
            for tmidx, tmname, tclass in dx.callees(code_off):
                if tmname in targets:
                    print('  %s#%s  ->  %s#%s' % (cls, mname, tclass, tmname))
                    found += 1
        if not found:
            print('  (无调用点)')


if __name__ == '__main__':
    apk = sys.argv[1]
    targets = set(sys.argv[2:]) or {'clearHistory', 'canGoBack', 'goBack'}
    scan(apk, ['classes.dex', 'classes2.dex', 'classes3.dex'], targets)
