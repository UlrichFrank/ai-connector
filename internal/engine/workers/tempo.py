# mlxlab's tempo helper (tempo lock, best of N): measures the tempo of songs near the tempo they were asked for.
#
#   python tempo.py <target bpm> <song.wav> [<song.wav> …]
#
# Prints one JSON object per song on stdout:
#   {"file": …, "tracked": librosa's beat-tracker tempo, "measured": the tempo of the strongest pulse within ±12 % of
#    the target (or of half / double the target, scaled back), "salience": that pulse's strength relative to the
#    strongest pulse between 50 and 300 bpm (1.0 = the dominant one), "ok": a clear pulse was found near the target}
#
# Why not the beat tracker alone: it often locks to a related tempo (2/3 of 174 for drum and bass, half time), and it
# quantises (≈ 1–2 bpm steps). The onset envelope's autocorrelation, searched near the target with a sub-lag peak
# fit, gives a precise tempo of the pulse that is actually there — and says when there is none.
import json
import sys
import warnings

warnings.filterwarnings("ignore")
import librosa  # noqa: E402
import numpy as np  # noqa: E402

SR, HOP = 22050, 128
WINDOW = 0.12  # how far from the target a pulse may be and still count (and be stretched)
MIN_SALIENCE = 0.5


def measure(path, target):
    y, sr = librosa.load(path, sr=SR, mono=True)
    onset = librosa.onset.onset_strength(y=y, sr=sr, hop_length=HOP)
    tracked = float(np.atleast_1d(librosa.beat.beat_track(onset_envelope=onset, sr=sr, hop_length=HOP)[0])[0])
    ac = librosa.autocorrelate(onset - onset.mean(), max_size=int(6 * sr / HOP))
    ac = ac / (ac[0] + 1e-12)
    fps = sr / HOP
    lags = np.arange(len(ac))
    bpm = np.where(lags > 0, 60.0 * fps / np.maximum(lags, 1), np.inf)
    span = (bpm >= 50) & (bpm <= 300)
    top = ac[span].max()
    best = None
    for k in (1.0, 0.5, 2.0):
        t = target * k
        sel = np.where((bpm >= t / (1 + WINDOW)) & (bpm <= t * (1 + WINDOW)))[0]
        if len(sel) < 3:
            continue
        i = sel[np.argmax(ac[sel])]
        if i in (sel[0], sel[-1]):  # the peak lies outside the window: no pulse near this tempo
            continue
        a, b, c = ac[i - 1], ac[i], ac[i + 1]  # parabolic fit for a sub-lag peak
        d = 0.5 * (a - c) / (a - 2 * b + c) if (a - 2 * b + c) != 0 else 0.0
        est = 60.0 * fps / (i + d) / k
        if best is None or b > best[1]:
            best = (est, b)
    if best is None:
        return {"file": path, "tracked": tracked, "measured": 0.0, "salience": 0.0, "ok": False}
    sal = float(best[1] / (top + 1e-12))
    return {"file": path, "tracked": tracked, "measured": float(best[0]), "salience": sal, "ok": sal >= MIN_SALIENCE}


target = float(sys.argv[1])
for p in sys.argv[2:]:
    print(json.dumps(measure(p, target)), flush=True)
