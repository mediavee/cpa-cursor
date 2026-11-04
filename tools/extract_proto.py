"""Rebuild .proto files from the Cursor CLI bundle's compact protobuf-es descriptors.

The bundle encodes each message as `static $(){return["Name|no name type[?*] [oneof]|...", refs...]}`
(see ../proto/dist/runtime/compact.js in the bundle) and each enum as
`(0,X.QT)(rt,"pkg.","Name",[[no,"NAME"],...],mode)`. Usage: extract_proto.py <bundle_dir> <out_dir>
Reads files only; never executes bundle code.
"""
import glob
import os
import re
import sys
from collections import defaultdict

SCALARS = {1: "double", 2: "float", 3: "int64", 4: "uint64", 5: "int32", 6: "fixed64", 7: "fixed32",
           8: "bool", 9: "string", 12: "bytes", 13: "uint32", 15: "sfixed32", 16: "sfixed64",
           17: "sint32", 18: "sint64"}
WKT_FILES = {"Struct": "struct", "Value": "struct", "ListValue": "struct", "NullValue": "struct"}
ID = r"[A-Za-z_$][\w$]*"
MOD_RE = re.compile(r'"((?:\.\./|\./)[^"]+\.(?:js|ts))"\(t,e,r\)\{')


def js_string(s, i):
    """Parse a JS double-quoted string starting at s[i]=='"'; return (value, end)."""
    assert s[i] == '"'
    out = []
    i += 1
    while s[i] != '"':
        if s[i] == "\\":
            nxt = s[i + 1]
            out.append({"n": "\n", "t": "\t"}.get(nxt, nxt))
            i += 2
        else:
            out.append(s[i])
            i += 1
    return "".join(out), i + 1


def split_modules(text):
    marks = list(MOD_RE.finditer(text))
    for k, m in enumerate(marks):
        end = marks[k + 1].start() if k + 1 < len(marks) else len(text)
        yield m.group(1), text[m.end():end]


def parse_exports(body):
    m = re.search(r"r\.d\(e,\{(.*?)\}\)", body)
    out = {}
    if m:
        for k, v in re.findall(r"(" + ID + r"|\"[^\"]+\"):\(\)=>(" + ID + r"(?:\." + ID + r")?)", m.group(1)):
            out[k.strip('"')] = v
    return out


def parse_imports(body):
    return {a: p for a, p in re.findall(r"(" + ID + r")=r\(\"([^\"]+)\"\)", body)}


def parse_refs(s, i):
    """Parse `,ref,ref]` after the descriptor string; returns list of ref expressions."""
    refs = []
    while s[i] == ",":
        m = re.match(ID + r"(?:\." + ID + r")?", s[i + 1:])
        refs.append(m.group(0))
        i += 1 + m.end()
    return refs


class Bundle:
    def __init__(self):
        self.modules = {}  # path -> dict(exports, imports, locals)
        self.types = {}  # full name -> dict
        self.services = []

    def load(self, text):
        for path, body in split_modules(text):
            mod = self.modules.setdefault(path, {"exports": {}, "imports": {}, "locals": {}, "bodies": []})
            mod["exports"].update(parse_exports(body))
            mod["imports"].update(parse_imports(body))
            mod["bodies"].append(body)
            consts = dict(re.findall(r"(?:const |var |,)(" + ID + r")=\"([a-z_]+(?:\.[a-z0-9_]+)*\.)\"", body))
            pkgs = {}
            for cls, lit, var in re.findall(r"class (" + ID + r") extends " + ID + r"(?:\." + ID + r")?\{static get runtime\(\)\{[^}]*\}static \$p\(\)\{return ?(?:\"([^\"]*)\"|(" + ID + r"))\}\}", body):
                pkgs[cls] = lit or consts.get(var)
            for m in re.finditer(r"class (" + ID + r") extends (" + ID + r")\{", body):
                var, base = m.group(1), m.group(2)
                j = body.find("static $(){return[", m.end())
                nxt = body.find("class ", m.end())
                if j < 0 or (nxt >= 0 and j > nxt and "static $(){return[" not in body[m.end():nxt]):
                    continue
                desc, k = js_string(body, j + len("static $(){return["))
                refs = parse_refs(body, k)
                pkg = pkgs.get(base)
                if pkg is None:
                    continue
                name = desc.split("|", 1)[0]
                mod["locals"][var] = ("msg", pkg + name, desc, refs, path)
            if "google/protobuf/" in path:
                for var, wkt in re.findall(r"(" + ID + r")\.typeName=\"(google\.protobuf\.\w+)\"", body):
                    mod["locals"][var] = ("wkt", wkt, None, None, "google/protobuf/" + WKT_FILES.get(wkt.split(".")[-1], wkt.split(".")[-1].lower()) + ".proto")
            for m in re.finditer(r"(" + ID + r")=\(0,(?:" + ID + r"|r\(\"[^\"]+\"\))\.QT\)\(" + ID + r"\.C,(?:\"([^\"]*)\"|(" + ID + r")),\"([^\"]*)\",\[", body):
                var, lit, cvar, name = m.groups()
                pkg = lit or consts.get(cvar)
                if pkg is None:
                    continue
                i = m.end() - 1
                depth = 0
                for k in range(i, len(body)):
                    if body[k] == "[":
                        depth += 1
                    elif body[k] == "]":
                        depth -= 1
                        if depth == 0:
                            break
                vals = [(int(n), v) for n, v in re.findall(r"\[(-?\d+),\"([^\"]*)\"\]", body[i:k + 1])]
                rest = body[k + 1:k + 4]
                mode1 = rest.startswith(",1)")
                short = name.rsplit(".", 1)[-1]
                prefix = (short[0] + re.sub(r"[A-Z]", lambda x: "_" + x.group(0), short[1:])).upper() + "_"
                values = [(n, prefix + v if mode1 else v) for n, v in vals]
                mod["locals"][var] = ("enum", pkg + name, values, path)
            for m in re.finditer(r"\{typeName:\"([\w.]+)\",methods:\{", body):
                start = m.end()
                depth, k = 1, start
                while depth:
                    depth += {"{": 1, "}": -1}.get(body[k], 0)
                    k += 1
                methods = re.findall(r"name:\"(\w+)\",I:(" + ID + r"(?:\." + ID + r")?),O:(" + ID + r"(?:\." + ID + r")?),kind:" + ID + r"\.I\.(\w+)", body[start:k])
                self.services.append((m.group(1), methods, path))

    def resolve(self, path, ref, depth=0):
        mod = self.modules.get(path)
        if mod is None or depth > 8:
            return None
        if "." in ref:
            alias, name = ref.split(".", 1)
            target = mod["imports"].get(alias)
            if target is None:
                return None
            tmod = self.modules.get(target)
            if tmod is None:
                return None
            local = tmod["exports"].get(name)
            if local is None:
                return None
            return self.resolve(target, local, depth + 1)
        loc = mod["locals"].get(ref)
        if loc:
            return loc
        return None

    def build(self):
        for path, mod in self.modules.items():
            for var, loc in mod["locals"].items():
                full = loc[1]
                if loc[0] == "wkt":
                    continue
                if loc[0] == "msg":
                    _, full, desc, refs, p = loc
                    fields = []
                    for part in desc.split("|")[1:]:
                        tok = part.split(" ")
                        no, fname, typ = int(tok[0]), tok[1], tok[2]
                        label = tok[3] if len(tok) > 3 else None
                        if typ[-1] in "?*":
                            label, typ = typ[-1], typ[:-1]
                        fields.append((no, fname, typ, label))
                    resolved = [self.resolve(path, r) for r in refs]
                    self.types[full] = {"kind": "msg", "fields": fields, "refs": resolved, "rawrefs": refs, "file": p}
                else:
                    self.types.setdefault(full, {"kind": "enum", "values": loc[2], "file": loc[3]})


def type_name(b, typ, refs):
    if typ.startswith("#"):
        r = refs[int(typ[1:])]
        if not r:
            return "bytes /* unresolved */", None
        return "." + r[1], (r[3] if r[0] == "enum" else r[4])
    return SCALARS.get(int(typ), "bytes"), None


def proto_file(path):
    if path.endswith(".proto"):
        return path
    p = re.sub(r"^(\.\./)+proto/dist/generated/", "", path)
    return re.sub(r"_pb\.js$", ".proto", p)


def emit(b, out_dir):
    # nested types whose parent message was tree-shaken out still need the parent scope
    for full in list(b.types):
        parts = full.split(".")
        for k in range(len(parts) - 1, 0, -1):
            parent = ".".join(parts[:k])
            if not parts[k - 1][:1].isupper() or parent in b.types:
                break
            b.types[parent] = {"kind": "msg", "fields": [], "refs": [], "rawrefs": [], "file": b.types[full]["file"], "stub": True}
    by_file = defaultdict(list)
    for full, t in b.types.items():
        by_file[t["file"]].append(full)
    for path, names in by_file.items():
        pkgs = {n.rsplit(".", 1)[0] if False else None for n in names}
        # package = longest common lowercase prefix segments
        segs = names[0].split(".")
        pkg = ".".join(s for s in segs if s and s[0].islower())
        imports = set()
        names_set = set(names)
        children = defaultdict(list)
        roots = []
        for n in sorted(names):
            parent = n.rsplit(".", 1)[0]
            (children[parent] if parent in names_set else roots).append(n)
        lines = []

        def render(n, ind):
            t = b.types[n]
            short = n.rsplit(".", 1)[-1] if n.rsplit(".", 1)[0] in names_set else n[len(pkg) + 1:].replace(".", "_")
            sp = "  " * ind
            if t["kind"] == "enum":
                lines.append(f"{sp}enum {short} {{")
                if not any(no == 0 for no, _ in t["values"]):
                    lines.append(f"{sp}  option allow_alias = true;")
                for no, v in t["values"]:
                    lines.append(f"{sp}  {v} = {no};")
                lines.append(f"{sp}}}")
                return
            if t.get("stub"):
                lines.append(f"{sp}// parent not present in the bundle; scope only")
            lines.append(f"{sp}message {short} {{")
            oneofs = defaultdict(list)
            plain = []
            for f in t["fields"]:
                no, fname, typ, label = f
                if "," in typ:
                    k, v = typ.split(",", 1)
                    vt, vf = type_name(b, v, t["refs"])
                    if vf and vf != path:
                        imports.add(vf)
                    plain.append(f"map<{SCALARS[int(k)]}, {vt}> {fname} = {no};")
                    continue
                tn, tf = type_name(b, typ, t["refs"])
                if tf and tf != path:
                    imports.add(tf)
                if label == "?":
                    plain.append(f"optional {tn} {fname} = {no};")
                elif label == "*":
                    plain.append(f"repeated {tn} {fname} = {no};")
                elif label:
                    oneofs[label].append(f"{tn} {fname} = {no};")
                else:
                    plain.append(f"{tn} {fname} = {no};")
            for x in plain:
                lines.append(f"{sp}  {x}")
            for o, fs in oneofs.items():
                lines.append(f"{sp}  oneof {o} {{")
                for x in fs:
                    lines.append(f"{sp}    {x}")
                lines.append(f"{sp}  }}")
            for c in children[n]:
                render(c, ind + 1)
            lines.append(f"{sp}}}")

        for r in roots:
            render(r, 0)
        svc_lines = []
        for sname, methods, spath in b.services:
            if not sname.startswith(pkg + ".") or spath != path:
                continue
        fname = proto_file(path)
        head = ['syntax = "proto3";', "", f"package {pkg};", ""]
        head += [f'import "{proto_file(i)}";' for i in sorted(imports)]
        dest = os.path.join(out_dir, fname)
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        with open(dest, "w") as fh:
            fh.write("\n".join(head + [""] + lines) + "\n")
    # services in their own files, resolved against the module that declares them
    svc_out = ['syntax = "proto3";', ""]
    seen = set()
    by_pkg = defaultdict(list)
    for sname, methods, spath in b.services:
        if sname in seen:
            continue
        seen.add(sname)
        by_pkg[sname.rsplit(".", 1)[0]].append((sname, methods, spath))
    for pkg, svcs in by_pkg.items():
        imports, body = set(), []
        for sname, methods, spath in svcs:
            body.append(f"service {sname.rsplit('.', 1)[1]} {{")
            for mname, i, o, kind in methods:
                ri, ro = b.resolve(spath, i), b.resolve(spath, o)
                def nm(r):
                    if not r:
                        return ".google.protobuf.Empty /* unresolved */"
                    imports.add(proto_file(r[4]))
                    return "." + r[1]
                cs = "stream " if kind in ("ClientStreaming", "BiDiStreaming") else ""
                ss = "stream " if kind in ("ServerStreaming", "BiDiStreaming") else ""
                body.append(f"  rpc {mname}({cs}{nm(ri)}) returns ({ss}{nm(ro)});")
            body.append("}")
        dest = os.path.join(out_dir, pkg.replace(".", "/"), "services.proto")
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        with open(dest, "w") as fh:
            fh.write("\n".join(['syntax = "proto3";', "", f"package {pkg};", ""] + [f'import "{x}";' for x in sorted(imports)] + [""] + body) + "\n")


def main():
    src, out = sys.argv[1], sys.argv[2]
    b = Bundle()
    for f in sorted(glob.glob(os.path.join(src, "*.js"))):
        b.load(open(f, encoding="utf-8", errors="replace").read())
    b.build()
    emit(b, out)
    unresolved = sum(1 for t in b.types.values() if t["kind"] == "msg" for r in t["refs"] if r is None)
    print(f"types={len(b.types)} services={len({s[0] for s in b.services})} unresolved_refs={unresolved}")


if __name__ == "__main__":
    main()
