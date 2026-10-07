// Is a song description electronic dance music? Then Stable Audio 3 Medium is the model to suggest: in the user's
// listening tests it sounded best by far for EDM (it is instrumental only).

const GENRES = [
  "edm", "trance", "techno", "house", "deep house", "tech house", "progressive house", "electro", "electronica",
  "drum and bass", "drum & bass", "drum'n'bass", "dnb", "d&b", "jungle", "dubstep", "brostep", "riddim", "synthwave",
  "retrowave", "outrun", "hardstyle", "hardcore", "gabber", "psytrance", "goa", "big room", "future bass", "garage",
  "uk garage", "breakbeat", "electro swing", "eurodance", "eurobeat", "minimal", "acid", "rave", "club", "dance",
  "idm", "trap edm", "melodic techno", "hard techno", "industrial techno",
];
const SOUNDS = ["four-on-the-floor", "four on the floor", "sidechain", "supersaw", "303", "808", "909", "drop", "build-up", "buildup", "reese"];

const word = (w: string) => new RegExp(`(^|[^a-z0-9])${w.replace(/[.*+?^${}()|[\]\\&']/g, "\\$&")}($|[^a-z0-9])`, "i");
const genreRes = GENRES.map((g) => [g, word(g)] as const);
const soundRes = SOUNDS.map((s) => [s, word(s)] as const);

/** The EDM words a description contains (genres first); empty when it doesn't read as EDM. */
export function edmWords(text: string): string[] {
  const t = text.toLowerCase();
  const genres = genreRes.filter(([, re]) => re.test(t)).map(([g]) => g);
  const sounds = soundRes.filter(([, re]) => re.test(t)).map(([s]) => s);
  // "dance" or "club" or "acid" alone is weak (dance pop, jazz club …): needs a second hint
  const weak = new Set(["dance", "club", "acid", "minimal", "hardcore", "garage", "drop"]);
  const strong = genres.filter((g) => !weak.has(g));
  if (strong.length) return [...genres, ...sounds];
  if (genres.length + sounds.length >= 2) return [...genres, ...sounds];
  return [];
}

export const isEDM = (text: string) => edmWords(text).length > 0;
