package pyodide

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTimezonesOffline(t *testing.T) {
	out, err := rt.Output(context.Background(), `
from datetime import datetime, timedelta, timezone
from importlib.metadata import version
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError, available_timezones, reset_tzpath
import tzdata

# Force the package fallback, independent of any system timezone files.
reset_tzpath(())
assert version("tzdata") == tzdata.__version__
assert tzdata.__file__.startswith("/usr/local/lib/tzdata.whl/"), tzdata.__file__

# Open every advertised zone, including aliases and nested packages.
zones = available_timezones()
assert len(zones) > 500, len(zones)
for name in zones:
    assert ZoneInfo(name).key == name

for name, winter, summer in [
    ("UTC", 0, 0),
    ("Europe/Zurich", 60, 120),
    ("America/New_York", -300, -240),
    ("Asia/Kathmandu", 345, 345),
    ("Australia/Sydney", 660, 600),
    ("America/Argentina/Buenos_Aires", -180, -180),
]:
    zone = ZoneInfo(name)
    for month, offset in [(1, winter), (7, summer)]:
        assert datetime(2026, month, 15, tzinfo=zone).utcoffset() == timedelta(minutes=offset), name

zurich = ZoneInfo("Europe/Zurich")
for utc, expected, fold in [
    ("2026-03-29T00:30:00+00:00", "2026-03-29T01:30:00+01:00", 0),
    ("2026-03-29T01:30:00+00:00", "2026-03-29T03:30:00+02:00", 0),
    ("2026-10-25T00:30:00+00:00", "2026-10-25T02:30:00+02:00", 0),
    ("2026-10-25T01:30:00+00:00", "2026-10-25T02:30:00+01:00", 1),
]:
    instant = datetime.fromisoformat(utc)
    local = instant.astimezone(zurich)
    assert local.isoformat() == expected, local
    assert local.fold == fold, local
    assert local.astimezone(timezone.utc) == instant

assert ZoneInfo("US/Eastern").utcoffset(datetime(2026, 7, 15)) == timedelta(hours=-4)
try:
    ZoneInfo("Missing/Timezone")
except ZoneInfoNotFoundError:
    pass
else:
    raise AssertionError("unknown zone accepted")

# The default lookup path also falls back to the bundled package.
reset_tzpath()
ZoneInfo.clear_cache()
assert datetime(2026, 7, 15, tzinfo=ZoneInfo("Europe/Zurich")).utcoffset() == timedelta(hours=2)
print("timezones ok")
`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "timezones ok\n" {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestInstalledTZDataPreferred(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx, WithCacheDir(filepath.Join(os.TempDir(), "go-pyodide-test-cache")))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	// An application can supply its own tzdata release without rebuilding Go.
	wheel := makeWheel(map[string]string{
		"tzdata/__init__.py":               "__version__ = '9999.1'\n",
		"tzdata-9999.1.dist-info/WHEEL":    "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"tzdata-9999.1.dist-info/METADATA": "Metadata-Version: 2.1\nName: tzdata\nVersion: 9999.1\n",
	})
	if err := r.AddWheelBytes(ctx, "tzdata-9999.1-py3-none-any.whl", wheel); err != nil {
		t.Fatal(err)
	}
	out, err := r.Output(ctx, `
import tzdata
from importlib.metadata import version
assert "/site-packages/" in tzdata.__file__, tzdata.__file__
assert tzdata.__version__ == version("tzdata") == "9999.1"
print("installed tzdata preferred")
`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "installed tzdata preferred\n" {
		t.Fatalf("unexpected output %q", out)
	}
}
