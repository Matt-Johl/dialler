#!/bin/sh
# In-place, idempotent source patches for baresip beyond the audiounit
# full-file replacements. Usage: apply-baresip.sh <baresip-src-dir>
#
# ua_refresh_register(): send a REGISTER now on the existing register
# clients. baresip's ua_register() destroys and re-creates its clients, and
# libre's client sends an un-REGISTER for the same contact as it is
# destroyed — which lands AFTER the new REGISTER and leaves the server with
# no binding (found by `make sim-call`). Refreshing in place (sipreg_send)
# has no such side effect.
set -eu
SRC="$1"

if ! grep -q 'reg_refresh' "$SRC/src/reg.c"; then
  perl -0pi -e 's/\nbool reg_isok\(const struct reg \*reg\)\n/\nint reg_refresh(struct reg *reg)\n{\n\tif (!reg)\n\t\treturn EINVAL;\n\n\tif (!reg->sipreg)\n\t\treturn ENOENT;\n\n\treturn sipreg_send(reg->sipreg);\n}\n\n\nbool reg_isok(const struct reg *reg)\n/' "$SRC/src/reg.c"
  grep -q 'reg_refresh' "$SRC/src/reg.c" || { echo "patch: reg.c anchor not found"; exit 1; }
fi

if ! grep -q 'reg_refresh' "$SRC/src/core.h"; then
  perl -pi -e 's/^(void reg_stop\(struct reg \*reg\);)$/$1\nint  reg_refresh(struct reg *reg);/' "$SRC/src/core.h"
  grep -q 'reg_refresh' "$SRC/src/core.h" || { echo "patch: core.h anchor not found"; exit 1; }
fi

if ! grep -q 'ua_refresh_register' "$SRC/src/ua.c"; then
  perl -0pi -e 's/(\t\treg_unregister\(reg\);\n\t\}\n\}\n)/$1\n\n\/**\n * Dialler: send a REGISTER now on every existing register client, keeping\n * the clients (so no un-REGISTER is sent on the way).\n *\n * \@param ua User-Agent\n *\n * \@return 0 if success, ENOENT if no register client exists, else errorcode\n *\/\nint ua_refresh_register(struct ua *ua)\n{\n\tstruct le *le;\n\tint err = ENOENT;\n\n\tif (!ua)\n\t\treturn EINVAL;\n\n\tfor (le = ua->regl.head; le; le = le->next) {\n\t\tstruct reg *reg = le->data;\n\t\tint e = reg_refresh(reg);\n\n\t\tif (!e)\n\t\t\terr = 0;\n\t\telse if (err == ENOENT)\n\t\t\terr = e;\n\t}\n\n\treturn err;\n}\n/' "$SRC/src/ua.c"
  grep -q 'ua_refresh_register' "$SRC/src/ua.c" || { echo "patch: ua.c anchor not found"; exit 1; }
fi

if ! grep -q 'ua_refresh_register' "$SRC/include/baresip.h"; then
  perl -pi -e 's/^(void ua_unregister\(struct ua \*ua\);)$/$1\nint  ua_refresh_register(struct ua *ua);/' "$SRC/include/baresip.h"
  grep -q 'ua_refresh_register' "$SRC/include/baresip.h" || { echo "patch: baresip.h anchor not found"; exit 1; }
fi
echo "   baresip: ua_refresh_register patch present"
