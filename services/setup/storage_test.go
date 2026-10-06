package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/datadir"
)

const gibs = int64(1) << 30

// storageUI answers the storage question from a queue, so a test can answer again after a refused size.
type storageUI struct {
	*fakeUI
	answers []string
}

func (u *storageUI) Text(ctx context.Context, q Question, prompt, initial string) (string, error) {
	if q == AskStorage && len(u.answers) > 0 {
		u.texts[q], u.answers = u.answers[0], u.answers[1:]
	}

	return u.fakeUI.Text(ctx, q, prompt, initial)
}

func newStorageUI(answers ...string) *storageUI {
	return &storageUI{fakeUI: &fakeUI{texts: map[Question]string{}}, answers: answers}
}

// onDisk makes every statfs read d, since a temp dir sits on whatever filesystem the machine has.
func onDisk(t *testing.T, d disk) {
	t.Helper()
	swap(t, &statDisk, func(string) (disk, error) { return d, nil })
}

// ext4 is a disk that cannot clone, with avail bytes free.
func ext4(avail int64) disk { return disk{total: 200 * gibs, used: 200*gibs - avail, avail: avail} }

func mib(gib int64) *int64 {
	m := gib << 10
	return &m
}

// image puts a data image of bytes at /var/lib/shard.xfs, sparse so it takes no space.
func (l *localHost) image(bytes int64) {
	l.t.Helper()
	l.write(datadir.ImagePath(DataDir), "")
	if err := os.Truncate(filepath.Join(l.root, datadir.ImagePath(DataDir)), bytes); err != nil {
		l.t.Fatalf("size the image: %v", err)
	}
}

func stopped(t *testing.T, err error, line string) {
	t.Helper()
	var stop *StoppedError
	var problem *Problem
	if !errors.As(err, &stop) || !errors.As(err, &problem) || !slices.Equal(problem.Lines, []string{line}) {
		t.Fatalf("err = %v, want a stop on %q", err, line)
	}
}

func TestTheStorageQuestionOffersHalfTheAvailableSpace(t *testing.T) {
	cases := []struct {
		name    string
		avail   int64
		initial string
		mib     int64
	}{
		{"half the available space", 120 * gibs, "60GiB", 60 << 10},
		{"capped at 100 GiB", 400 * gibs, "100GiB", 100 << 10},
		{"rounded down to whole GiB", 41*gibs + gibs/2, "20GiB", 20 << 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			onDisk(t, ext4(tc.avail))
			ui := newStorageUI(tc.initial)

			got, err := (&Setup{Host: l.host(), UI: ui}).storageSize(t.Context(), Local{Provider: Firecracker, StartAtBoot: true})
			if err != nil {
				t.Fatalf("storageSize = %v; printed %q", err, ui.printed)
			}

			if !slices.Equal(ui.asked, []Question{AskStorage}) || !slices.Equal(ui.initials, []string{tc.initial}) {
				t.Fatalf("asked %v with %q, want the storage question with %s", ui.asked, ui.initials, tc.initial)
			}
			if got.StorageMiB != tc.mib {
				t.Fatalf("StorageMiB = %d, want %d", got.StorageMiB, tc.mib)
			}
			if l.changed() {
				t.Fatalf("the question changed the host: %q", l.calls)
			}
		})
	}
}

func TestTheStorageQuestionShowsTheSpaceBeforeItAsks(t *testing.T) {
	l := newLocalHost(t)
	onDisk(t, ext4(120*gibs))
	ui := newStorageUI("40GiB")

	if _, err := (&Setup{Host: l.host(), UI: ui}).storageSize(t.Context(), Local{Provider: Firecracker, StartAtBoot: true}); err != nil {
		t.Fatalf("storageSize = %v", err)
	}

	want := []string{
		"", "Sandbox storage", "",
		"The filesystem of /var/lib: 200.0 GiB in total, 80.0 GiB used, 120.0 GiB available.",
		"It cannot clone a disk, so Firecracker keeps sandbox disks in an XFS image, /var/lib/shard.xfs.",
		"shard reserves the whole size at once, when setup starts the daemon. The host cannot use that space, even while sandboxes leave it empty.",
		"Each Firecracker sandbox has a 10 GiB disk limit by default. Choose a smaller limit with shard create --disk.",
		"Enter a size from 10 GiB to 110 GiB, in KiB, MiB, GiB, KB, MB or GB. The default is half the available space, at most 100 GiB.",
		"",
	}
	if !slices.Equal(ui.printed, want) {
		t.Fatalf("printed\n%q\nwant\n%q", ui.printed, want)
	}
}

func TestTheStorageQuestionAsksAgainAfterARefusedSize(t *testing.T) {
	l := newLocalHost(t)
	onDisk(t, ext4(120*gibs))
	ui := newStorageUI("200GB", "5GiB", "12XB", "1.5GiB", "", "0", " 40GiB ")

	got, err := (&Setup{Host: l.host(), UI: ui}).storageSize(t.Context(), Local{Provider: Firecracker})
	if err != nil {
		t.Fatalf("storageSize = %v; printed %q", err, ui.printed)
	}

	if got.StorageMiB != 40<<10 || len(ui.asked) != 7 {
		t.Fatalf("StorageMiB = %d after %d questions, want 40 GiB after 7", got.StorageMiB, len(ui.asked))
	}
	said(t, ui.fakeUI,
		"Only 120.0 GiB is available, and shard keeps 10 GiB of it for the host, so the most it can reserve is 110 GiB.",
		"5 GiB is below the minimum of 10 GiB.",
		`Unknown unit "XB"; want KiB, MiB, GiB, KB, MB or GB.`,
		"Want a whole number; a fraction is never rounded.",
		"Want a whole size such as 512MiB or 2GiB.",
		"0 MiB is below the minimum of 10 GiB.",
		"when you first start the daemon.",
	)
}

func TestStorageSizeSkipsTheQuestion(t *testing.T) {
	cases := []struct {
		name string
		flag *int64
		want string
	}{
		{name: "a size that fits", flag: mib(50)},
		{name: "too large", flag: mib(500), want: "--storage-size 500GiB: only 120.0 GiB is available, and shard keeps 10 GiB of it for the host, so the most it can reserve is 110 GiB."},
		{name: "too small", flag: mib(5), want: "--storage-size 5GiB: 5 GiB is below the minimum of 10 GiB."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			onDisk(t, ext4(120*gibs))
			ui := newStorageUI()

			got, err := (&Setup{Host: l.host(), UI: ui, StorageMiB: tc.flag}).storageSize(t.Context(), Local{Provider: Firecracker})

			if len(ui.asked) != 0 || l.changed() {
				t.Fatalf("the flag asked %v and ran %q", ui.asked, l.calls)
			}
			if tc.want == "" {
				if err != nil || got.StorageMiB != *tc.flag {
					t.Fatalf("storageSize = %d, %v; want %d", got.StorageMiB, err, *tc.flag)
				}
				return
			}
			stopped(t, err, tc.want)
			said(t, ui.fakeUI, tc.want, "No installation changes were made.")
		})
	}
}

func TestStorageStopsBelowTheSmallestImage(t *testing.T) {
	l := newLocalHost(t)
	onDisk(t, ext4(15*gibs))
	ui := newStorageUI()

	_, err := (&Setup{Host: l.host(), UI: ui}).storageSize(t.Context(), Local{Provider: Firecracker})

	stopped(t, err, "Only 15.0 GiB is available, and shard keeps 10 GiB of it for the host.")
	if len(ui.asked) != 0 {
		t.Fatalf("asked %v with no size to offer", ui.asked)
	}
}

func TestStorageSizeRefusesWhereNothingIsReserved(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		mac      bool
		reflink  bool
		want     string
	}{
		{name: "gVisor", provider: GVisor, want: "--storage-size 50GiB: gVisor reserves no space: it keeps sandbox data in /var/lib/shard as it grows."},
		{name: "runc", provider: Runc, want: "--storage-size 50GiB: " + providerTitle(Runc) + " reserves no space: it keeps sandbox data in /var/lib/shard as it grows."},
		{name: "vz", provider: VZ, mac: true, want: "--storage-size 50GiB: " + providerTitle(VZ) + " reserves no space: it keeps sandbox data in /var/lib/shard as it grows."},
		{name: "Firecracker on XFS", provider: Firecracker, reflink: true, want: "--storage-size 50GiB: /var/lib/shard is on a filesystem that clones a disk, so Firecracker makes no data image there and reserves no space."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			onDisk(t, disk{total: 200 * gibs, avail: 120 * gibs, reflink: tc.reflink})
			h := l.host()
			if tc.mac {
				h = l.mac()
			}

			ui := newStorageUI()
			got, err := (&Setup{Host: h, UI: ui}).storageSize(t.Context(), Local{Provider: tc.provider})
			if err != nil || got.StorageMiB != 0 || len(ui.asked) != 0 || len(ui.printed) != 0 {
				t.Fatalf("without the flag: %d, %v; asked %v, printed %q", got.StorageMiB, err, ui.asked, ui.printed)
			}

			ui = newStorageUI()
			_, err = (&Setup{Host: h, UI: ui, StorageMiB: mib(50)}).storageSize(t.Context(), Local{Provider: tc.provider})
			stopped(t, err, tc.want)
		})
	}
}

func TestStorageShowsAnImageAndNeverResizesIt(t *testing.T) {
	const size = 98971756544
	cases := []struct {
		name string
		flag *int64
		want string
	}{
		{name: "no flag"},
		{name: "the flag names its size, rounded up to whole MiB", flag: func() *int64 { m := int64(94387); return &m }()},
		{name: "a new size", flag: mib(50), want: "--storage-size 50GiB: this installation already reserves 92.2 GiB, and setup does not resize it."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			l.image(size)
			onDisk(t, ext4(120*gibs))
			ui := newStorageUI()

			got, err := (&Setup{Host: l.host(), UI: ui, StorageMiB: tc.flag}).storageSize(t.Context(), Local{Provider: Firecracker})

			said(t, ui.fakeUI, "Storage: 92.2 GiB in /var/lib/shard.xfs. Setup does not resize it.")
			if len(ui.asked) != 0 || got.StorageMiB != 0 {
				t.Fatalf("asked %v and chose %d over an image", ui.asked, got.StorageMiB)
			}
			if tc.want == "" && err != nil {
				t.Fatalf("storageSize = %v", err)
			}
			if tc.want != "" {
				stopped(t, err, tc.want)
			}
		})
	}
}

func TestTheExistingSummaryShowsTheStorage(t *testing.T) {
	cases := []struct {
		name     string
		image    int64
		storage  int64
		want     string
		reserves string
	}{
		{name: "an image", image: 50 * gibs, want: "Storage:  50.0 GiB in /var/lib/shard.xfs", reserves: "50.0 GiB"},
		{name: "a size the daemon has not reserved yet", storage: 40 << 10, want: "Storage:  40.0 GiB, reserved when the daemon first starts", reserves: "40.0 GiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			m.Provider, m.StorageMiB = Firecracker, tc.storage
			f.installed(t, m)
			if tc.image > 0 {
				f.write(t, datadir.ImagePath(DataDir), "")
				if err := os.Truncate(filepath.Join(f.root, datadir.ImagePath(DataDir)), tc.image); err != nil {
					t.Fatalf("size the image: %v", err)
				}
			}

			ui := &fakeUI{selects: map[Question]string{AskExisting: "exit"}}
			if err := (&Setup{Host: f.host(nil), UI: ui}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceActive}); err != nil {
				t.Fatalf("existing: %v", err)
			}
			if !slices.Contains(ui.printed, tc.want) {
				t.Fatalf("summary %q lacks %q", ui.printed, tc.want)
			}

			ui = &fakeUI{}
			err := (&Setup{Host: f.host(nil), UI: ui, StorageMiB: mib(60)}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceActive})
			stopped(t, err, "--storage-size 60GiB: this installation already reserves "+tc.reserves+", and setup does not resize it.")
			if len(ui.asked) != 0 || len(f.calls) != 0 {
				t.Fatalf("a new size reached the menu: asked %v, ran %v", ui.asked, f.calls)
			}
		})
	}
}

func TestTheExistingSummaryOfGVisorRefusesAStorageSize(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	ui := &fakeUI{}

	err := (&Setup{Host: f.host(nil), UI: ui, StorageMiB: mib(60)}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceActive})

	stopped(t, err, "--storage-size 60GiB: gVisor reserves no space: it keeps sandbox data in /var/lib/shard as it grows.")
	if slices.ContainsFunc(ui.printed, func(p string) bool { return strings.HasPrefix(p, "Storage:") }) || len(ui.asked) != 0 {
		t.Fatalf("printed %q, asked %v", ui.printed, ui.asked)
	}
}

func TestTheChosenSizeReachesTheDaemon(t *testing.T) {
	l := Local{Provider: Firecracker, StorageMiB: 50 << 10}
	if unit := systemdUnitText(l); !strings.Contains(unit, "ExecStart=/usr/local/bin/shard daemon --provider firecracker --storage-size 50GiB\n") {
		t.Fatalf("the unit does not size the image:\n%s", unit)
	}
	if unit := systemdUnitText(Local{Provider: GVisor}); strings.Contains(unit, "--storage-size") {
		t.Fatalf("the gVisor unit sizes an image:\n%s", unit)
	}
	if done := localDone(Host{OS: "linux", Version: "v0.1.0", Env: sudoUser}, l, nil); !slices.Contains(done, "    sudo shard daemon --provider firecracker --storage-size 50GiB") {
		t.Fatalf("the done text names no size: %q", done)
	}

	h := Host{Root: t.TempDir(), OS: "linux", Env: sudoUser}
	data, err := json.Marshal(Manifest{Version: "v0.1.1", Provider: Firecracker, StorageMiB: 50 << 10})
	if err != nil {
		t.Fatal(err)
	}
	put(t, h, ManifestPath, data)
	if got, err := LocalDaemon(h); err != nil || got.Hint != "is it running? sudo shard daemon --provider firecracker --storage-size 50GiB" {
		t.Fatalf("LocalDaemon = %+v, %v", got, err)
	}
}

func TestTheManifestKeepsTheStorageSize(t *testing.T) {
	f := newFakeHost(t)
	h := f.host(nil)

	if err := RecordOwned(t.Context(), h, Local{Provider: Firecracker, StorageMiB: 50 << 10}, Owned{Path: "/usr/local/bin/shard", Kind: KindBinary}); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}

	m, ok, err := LoadManifest(h)
	if err != nil || !ok || m.StorageMiB != 50<<10 {
		t.Fatalf("LoadManifest = %+v, %v, %v; want 50 GiB of storage", m, ok, err)
	}
	raw, found := f.read(t, ManifestPath)
	if !found || !strings.Contains(raw, `"storage_mib": 51200`) {
		t.Fatalf("the manifest holds %s", raw)
	}
}

func TestTheReviewShowsWhatTheHostKeeps(t *testing.T) {
	l := newLocalHost(t)
	ui := newStorageUI()
	ui.confirms = map[Question]bool{AskConfirm: false}

	err := (&Setup{Host: l.host(), UI: ui}).review(t.Context(), Local{Provider: Firecracker, StorageMiB: 50 << 10, avail: 120 * gibs}, "")

	if err == nil {
		t.Fatal("a declined review went on")
	}
	said(t, ui.fakeUI, "Storage:           50 GiB, in /var/lib/shard.xfs", "Host space left:   70.0 GiB")
}

func TestTheDaemonStartRechecksTheSpace(t *testing.T) {
	cases := []struct {
		name  string
		mib   int64
		image bool
		avail int64
		want  string
	}{
		{name: "no size", avail: gibs},
		{name: "room", mib: 50 << 10, avail: 120 * gibs},
		{name: "an image already", mib: 50 << 10, image: true, avail: gibs},
		{name: "the disk filled", mib: 50 << 10, avail: 40 * gibs, want: "shard cannot reserve 50 GiB now: only 40.0 GiB is available, and shard keeps 10 GiB of it for the host, so the most it can reserve is 30 GiB."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			onDisk(t, ext4(tc.avail))
			if tc.image {
				l.image(50 * gibs)
			}

			err := roomAtStart(l.host(), tc.mib)

			if tc.want == "" && err != nil {
				t.Fatalf("roomAtStart = %v", err)
			}
			var problem *Problem
			if tc.want != "" && (!errors.As(err, &problem) || !slices.Equal(problem.Lines, []string{tc.want})) {
				t.Fatalf("roomAtStart = %v, want %q", err, tc.want)
			}
		})
	}
}
