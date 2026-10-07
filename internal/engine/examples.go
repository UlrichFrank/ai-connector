package engine

// Example is a ready-made request for trying a kind of model: the web app and the TUI offer them to pick from.
type Example struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt,omitempty"` // chat, image, video
	Size   string `json:"size,omitempty"`   // image, video: WxH
	Frames int    `json:"frames,omitempty"` // video
	// video: a movie, clip by clip (Prompt: what every clip shares — characters, look)
	Script []Clip `json:"script,omitempty"`
	// music: Prompt is the style
	Lyrics   string   `json:"lyrics,omitempty"`
	Duration float64  `json:"duration,omitempty"`
	Texts    []string `json:"texts,omitempty"` // embed; classify: the texts asked about
	// classify
	Question string   `json:"question,omitempty"`
	Options  []Option `json:"options,omitempty"`
	// speech: Text is said by Voice (a preset; "" the model's default), as Instruct says, in Language;
	// avatar: what the portrait says (spoken by a speech model; Prompt: the scene and motion, Size: WxH)
	Text     string `json:"text,omitempty"`
	Voice    string `json:"voice,omitempty"`
	Instruct string `json:"instruct,omitempty"`
	Language string `json:"language,omitempty"`
}

// Examples per kind.
var Examples = map[Kind][]Example{
	Chat: {
		{Title: "Explain simply", Prompt: "Explain how a Mixture-of-Experts language model works to a curious 12-year-old, in five sentences."},
		{Title: "Reasoning puzzle", Prompt: "A bat and a ball cost 1.10 € in total. The bat costs 1.00 € more than the ball. How much does the ball cost? Show your reasoning briefly."},
		{Title: "Go code", Prompt: "Write a Go function that returns the n most frequent words of a text, case-insensitive, with a table-driven test."},
		{Title: "Translate (DE)", Prompt: "Translate into natural German, keep the tone: \"Thanks for jumping on this so quickly — let's sync tomorrow and ship it by Friday.\""},
		{Title: "Summarize", Prompt: "Summarize the trade-offs between running AI models locally on a laptop versus in the cloud, as a short bullet list."},
		{Title: "JSON extraction", Prompt: "Extract name, date and amount as JSON from: \"On 3 March 2026 Ada Lovelace paid 1,250.50 EUR for the conference tickets.\" Answer with JSON only."},
		{Title: "Creative", Prompt: "Write a four-line poem about a Mac quietly generating images at night."},
		{Title: "Long context recall", Prompt: "List the planets of the solar system with one surprising fact each, then tell me which fact you find most surprising and why."},
	},
	Image: {
		{Title: "Photo: fox in snow", Prompt: "A red fox sitting in fresh snow at golden hour, shallow depth of field, 85mm photo, soft backlight", Size: "1024x1024"},
		{Title: "Product shot", Prompt: "Minimalist studio product photo of a matte black ceramic coffee cup on a pale concrete surface, soft shadows, top light", Size: "1024x1024"},
		{Title: "Typography", Prompt: "A vintage travel poster for the city of Hamburg with the bold text \"HAMBURG\" at the top, harbour cranes and ships, flat colours, 1960s style", Size: "768x1344"},
		{Title: "Interior", Prompt: "A cozy reading nook in a treehouse, warm evening light, rain on the windows, plants and stacked books, film photo", Size: "1344x768"},
		{Title: "Illustration", Prompt: "Isometric illustration of a tiny data centre on a floating island, pastel colours, clean vector style, soft gradients", Size: "1024x1024"},
		{Title: "Portrait", Prompt: "Portrait of an elderly fisherman with a weathered face and a knitted cap, overcast light, Kodak Portra film look", Size: "896x1152"},
		{Title: "Edit: style transfer", Prompt: "Turn this image into a watercolour painting with loose brush strokes, keep the composition", Size: "1024x1024"},
		{Title: "Edit: change scene", Prompt: "Keep the main subject exactly as it is, but place it on a beach at sunset", Size: "1024x1024"},
	},
	Video: {
		{Title: "Door opens", Prompt: "A heavy wooden door creaks slowly open onto a sunlit garden, dust floating in the light, birds singing", Size: "704x448", Frames: 97},
		{Title: "Ocean waves", Prompt: "Waves crashing against black volcanic rocks at sunset, slow motion spray, the roar of the surf", Size: "704x448", Frames: 97},
		{Title: "City at night", Prompt: "A rainy neon-lit street in Tokyo at night, camera slowly dollies forward past people with umbrellas, distant traffic sounds", Size: "704x448", Frames: 121},
		{Title: "Cooking", Prompt: "Close-up of butter melting and sizzling in a cast-iron pan, garlic is added and starts to brown, kitchen sounds", Size: "704x448", Frames: 73},
		{Title: "Talking", Prompt: "A friendly barista looks into the camera and says \"Your coffee is ready!\" with a smile, café ambience in the background", Size: "448x704", Frames: 97},
		{Title: "Quick test (fast)", Prompt: "A candle flame flickering in a dark room", Size: "512x320", Frames: 25},

		// movies
		{Title: "Movie: the fox, the squirrel and the acorn", Size: "704x448", Frames: 97,
			Prompt: "Funny 2D cartoon in a bright, colorful Saturday-morning style with bold outlines. Characters: a lanky orange fox with a big fluffy tail and a green scarf, and a tiny hyperactive grey squirrel with huge eyes. Sunny autumn forest, cheerful slapstick music",
			Script: []Clip{
				{Prompt: "Wide shot: the squirrel proudly carries a giant golden acorn across a forest clearing, whistling a happy tune."},
				{Prompt: "The fox peeks out from behind a tree, licks his lips and tiptoes after the squirrel in an exaggerated sneaky walk.", Continue: true},
				{Prompt: "Close-up: the fox lunges for the acorn, but the squirrel zips up a tree trunk in a blur and the fox face-plants into a pile of leaves with a comical thud.", Continue: true},
				{Prompt: "From a branch above, the squirrel laughs and drops the acorn, which bonks the fox on the head with a cartoon boing; little stars circle his head.", Continue: true},
				{Prompt: "New scene at sunset: the fox and the squirrel sit side by side on a log, sharing the acorn and laughing together, leaves drifting down."},
			}},
		{Title: "Movie: coffee commercial", Size: "704x448", Frames: 73,
			Prompt: "Warm cinematic product commercial, shallow depth of field, soft morning light, gentle acoustic music",
			Script: []Clip{
				{Prompt: "Macro shot of roasted coffee beans tumbling in slow motion onto a wooden table."},
				{Prompt: "A barista pours steaming milk into a cup, drawing a heart in the latte art.", Continue: false},
				{Prompt: "The camera slowly pulls back from the cup as a hand lifts it towards a sunlit window.", Continue: true},
				{Prompt: "A young woman by the window takes a sip, closes her eyes and smiles; the steam curls in the light.", Continue: true},
			}},
		{Title: "Movie: storm over the sea (one long shot)", Size: "704x448", Frames: 97,
			Prompt: "Photorealistic nature documentary, dramatic lighting, wind and thunder sounds",
			Script: []Clip{
				{Prompt: "A calm turquoise sea under a blue sky, gulls gliding over gentle waves."},
				{Prompt: "Dark clouds roll in over the horizon, the wind picks up and the waves grow taller.", Continue: true},
				{Prompt: "Lightning strikes the sea as rain pours down and huge waves crash against black rocks.", Continue: true},
				{Prompt: "The storm passes; golden sunlight breaks through the clouds and a rainbow appears over the calm sea.", Continue: true},
			}},
	},
	Music: {
		{Title: "Indie pop", Prompt: "indie pop, bright acoustic guitar, soft drums, warm female lead vocal, 110 bpm", Duration: 60,
			Lyrics: "[verse]\nSoft morning light is touching the window\nCoffee is warm and the city is slow\n\n[chorus]\nStay with the rhythm, let it carry us home\nWe were never really out here alone"},
		{Title: "Synthwave (instrumental)", Prompt: "synthwave, retro 80s, driving bass, arpeggiated synths, gated reverb drums, 118 bpm", Duration: 45},
		{Title: "Christmas", Prompt: "christmas pop, sleigh bells, piano, choir backing, cheerful male vocal", Duration: 60,
			Lyrics: "[verse]\nSnow on the rooftops, lights in the street\nCandles are glowing, the cookies are sweet\n\n[chorus]\nIt's Christmas tonight, come home to the light\nEverything's right on a Christmas night"},
		{Title: "Halloween", Prompt: "spooky rock, minor key, organ, heavy guitars, theatrical male vocal, 140 bpm", Duration: 45,
			Lyrics: "[verse]\nPumpkins are grinning on every stair\nSomething is moving, there's frost in the air\n\n[chorus]\nHalloween, Halloween, nothing is what it seems"},
		{Title: "German Schlager", Prompt: "German schlager, upbeat, accordion, four on the floor, female vocal", Duration: 60,
			Lyrics: "[verse]\nDie Sonne scheint auf unser Dach\nWir tanzen bis die Nacht erwacht\n\n[chorus]\nHeute ist ein schöner Tag\nWeil ich dich so gerne mag"},
		{Title: "Progressive house", Prompt: "progressive house, euphoric, 124 bpm, four-on-the-floor kick drum, warm rolling bassline, plucked synth arpeggios, wide pads, airy female vocal, festival sunset", Duration: 120,
			Lyrics: "[intro]\n\n[verse]\nCity lights are fading in the rear view\nEvery road is leading back to you\n\n[build-up]\nHold on, hold on, the night is turning gold\n\n[drop]\n\n[verse]\nWe were chasing echoes in the blue\nNow the sky is breaking into view\n\n[chorus]\nWe are the sunrise, we are the open sea\nWe are the sunrise, finally free\n\n[outro]"},
		{Title: "Trance anthem", Prompt: "uplifting trance, 138 bpm, four-on-the-floor kick drum, supersaw lead, rolling offbeat bass, big breakdown, ethereal female vocal, emotional", Duration: 150,
			Lyrics: "[intro]\n\n[verse]\nI see the stars beneath my feet\nThe world is quiet, the night is sweet\n\n[breakdown]\nTake my hand and close your eyes\nWe're flying higher than the skies\n\n[build-up]\nHigher, higher, higher\n\n[drop]\n\n[chorus]\nWe are light, we are the dream\nNothing's ever what it seems\n\n[outro]"},
		{Title: "Dance pop", Prompt: "dance pop, 122 bpm, four on the floor, funky bass, bright synth stabs, catchy female vocal, radio hit", Duration: 90,
			Lyrics: "[verse]\nFriday night and my phone is lighting up\nGot my shoes on, I can't get enough\n\n[pre-chorus]\nTurn it up, turn it up, let the bassline drop\n\n[chorus]\nDance with me until the morning comes\nMove your feet to the beat of the drums\nNo one's going home tonight\nDance with me under neon light\n\n[verse]\nEvery heartbeat's matching with the kick\nEvery moment's sweeter than the last\n\n[chorus]\nDance with me until the morning comes\nMove your feet to the beat of the drums"},
		{Title: "Eurodance 90s", Prompt: "90s eurodance, 135 bpm, energetic, rap verse by male voice, soaring female chorus, rave synths, retro", Duration: 90,
			Lyrics: "[verse]\nYo, check it, the rhythm's in control\nFeel the energy deep inside your soul\nHands up high, let the music take the lead\nEverybody now, this is what we need\n\n[chorus]\nOh oh, take me to the light\nOh oh, we can fly tonight\nNever let me go, never let me fall\nTogether we are standing tall"},
		{Title: "Deep house", Prompt: "deep house, 120 bpm, smooth, warm sub bass, jazzy rhodes chords, crisp shuffled hats, soulful male vocal, late night", Duration: 90,
			Lyrics: "[intro]\n\n[verse]\nSlow down, the city's breathing low\nLet the rhythm take us where we go\n\n[chorus]\nIn the deep, in the deep of the night\nYou and me in the low blue light\n\n[outro]"},
		{Title: "Techno (instrumental)", Prompt: "dark techno, 130 bpm, pounding four-on-the-floor kick drum, driving hi-hats, rumbling bass, metallic percussion, acid 303 line, warehouse, electronic", Duration: 90},
		// EDM instrumentals as the EDM benchmark prompted them (docs/music.md): tag style, the tempo as "NNN bpm"
		// (passed to the model as its tempo), the rhythm section spelled out
		{Title: "Uplifting trance (instrumental)", Prompt: "uplifting trance, 138 bpm, four-on-the-floor kick drum, punchy kick, rolling offbeat bass, supersaw lead, driving hi-hats, energetic, electronic", Duration: 90},
		{Title: "Progressive house (instrumental)", Prompt: "progressive house, 124 bpm, four-on-the-floor kick drum, punchy kick, rolling bassline, plucked synth arpeggios, wide pads, electronic", Duration: 90},
		{Title: "Drum and bass (instrumental)", Prompt: "drum and bass, 174 bpm, fast breakbeat drums, punchy kick and snare, deep reese bass, energetic, electronic", Duration: 90},
		{Title: "Long DJ track (instrumental, Stable Audio 3 Medium)", Prompt: "deep tech house, 124 bpm, four-on-the-floor kick drum, shuffled hi-hats, warm rolling bassline, hypnotic chord stabs, DJ-friendly intro and outro, electronic", Duration: 300},
		{Title: "Drum and bass", Prompt: "liquid drum and bass, 174 bpm, rolling breakbeats, deep reese bass, lush pads, female vocal, uplifting", Duration: 90,
			Lyrics: "[verse]\nRunning through the rain at midnight\nEvery drop is shining like a starlight\n\n[chorus]\nWe keep on moving, we never stop\nRiding the rhythm, straight to the top\n\n[drop]"},
		{Title: "Synthpop 80s", Prompt: "80s synthpop, 112 bpm, analog synths, gated reverb snare, chorus guitar, nostalgic male vocal", Duration: 90,
			Lyrics: "[verse]\nPolaroids and cassette tapes\nSummer nights on the interstate\n\n[chorus]\nWe were young and the radio was loud\nDancing hearts in a neon crowd\nHold that moment, don't let it fade\nForever young in the arcade"},
		{Title: "Big room EDM (instrumental)", Prompt: "big room EDM, 128 bpm, festival main stage, massive four-on-the-floor kick drum, huge lead drop, snare roll build-up, crowd energy", Duration: 60},
		{Title: "Chillout / ambient (instrumental)", Prompt: "chillout ambient, 90 bpm, soft pads, gentle piano, ocean waves, warm sub, relaxing, balearic", Duration: 90},
		{Title: "Rock ballad", Prompt: "rock ballad, 72 bpm, emotional, clean electric guitar arpeggios, power chords in the chorus, strings, raspy male vocal", Duration: 120,
			Lyrics: "[verse]\nI kept your letters in a box beneath my bed\nEvery word you never said is spinning in my head\n\n[chorus]\nAnd I will wait for you\nThrough the storm and the morning dew\nIf the stars all fall from the sky\nI'll still be here, I won't say goodbye\n\n[bridge]\nTime is a river, it carries us on\n\n[chorus]\nAnd I will wait for you"},
		{Title: "Hip hop", Prompt: "boom bap hip hop, 90 bpm, dusty drums, jazzy piano sample, deep bass, confident male rap", Duration: 60,
			Lyrics: "[verse]\nWoke up early with a pen in my hand\nWriting down the vision, yeah, I got a plan\nCity never sleeps and neither do I\nChasing every dream till I touch the sky\n\n[chorus]\nWe on the rise, we on the rise\nCan't stop now, see it in my eyes"},
		{Title: "Reggaeton", Prompt: "reggaeton, 95 bpm, dembow rhythm, latin pop, warm synth bass, male and female vocals in Spanish, summer", Duration: 90,
			Lyrics: "[verse]\nLa noche está caliente, ven conmigo a bailar\nLa luna está brillando sobre el mar\n\n[chorus]\nBaila, baila, no pares de bailar\nQue esta noche nadie nos va a parar"},
		{Title: "Deutscher Pop", Prompt: "German pop, 100 bpm, acoustic guitar, warm piano, light drums, heartfelt female vocal", Duration: 90,
			Lyrics: "[verse]\nIch hab die Straßen nachts gezählt\nUnd jeden Stern, der mir gefehlt\n\n[chorus]\nUnd wenn die Welt sich weiterdreht\nBleib ich hier, bis du verstehst\nDass jedes Wort, das ich dir schrieb\nNoch immer in mir bleibt"},
		{Title: "Lo-fi beat (quick test)", Prompt: "lo-fi hip hop, mellow piano, vinyl crackle, relaxed, 80 bpm", Duration: 20},
	},
	Speech: {
		{Title: "Guten Abend (DE)", Language: "de", Text: "Guten Abend! Das ist ein Test der lokalen Sprachausgabe."},
		{Title: "Nachrichten (DE)", Language: "de", Instruct: "ruhig und sachlich, wie ein Nachrichtensprecher",
			Text: "Hamburg. Am Sonntag bleibt es überwiegend trocken, im Norden ziehen am Nachmittag einzelne Schauer durch. " +
				"Die Temperaturen erreichen bis zu 14 Grad. Zum Wochenstart wird es dann spürbar kühler."},
		{Title: "Märchen (DE)", Language: "de", Instruct: "warm, slow, like reading a bedtime story to a child",
			Text: "Es war einmal ein kleiner Fuchs, der in einem Wald voller hoher Tannen lebte. Jeden Abend saß er auf einem Hügel und zählte die Sterne.\n\n" +
				"Eines Nachts fiel ein Stern direkt vor seine Pfoten. „Wer bist du?“, fragte der Fuchs leise."},
		{Title: "Excited announcement (EN)", Language: "en", Instruct: "very excited and happy, energetic",
			Text: "We did it! The new version is finally out, and it is faster than anything we have built before. Thank you all so much!"},
		{Title: "Calm narration (EN)", Language: "en", Instruct: "calm, warm, a little amused",
			Text: "The Mac hummed quietly in the corner. Somewhere inside it, a small model was learning to speak, one sentence at a time."},
		{Title: "Whisper (EN)", Language: "en", Instruct: "whispering, secretive",
			Text: "Come closer. I have a little secret to share, and nobody else may hear it."},
		{Title: "Voxtral preset (DE)", Language: "de", Voice: "de_male",
			Text: "Willkommen zu unserem Podcast. Heute sprechen wir darüber, wie man künstliche Intelligenz ganz lokal auf dem eigenen Rechner nutzt."},
	},
	Sound: {
		{Title: "Rain on a tin roof", Prompt: "steady rain drumming on a tin roof, close and detailed, occasional heavier drops", Duration: 10},
		{Title: "Creaking door", Prompt: "an old wooden door creaking open slowly in a quiet room", Duration: 5},
		{Title: "Stadium crowd", Prompt: "a huge crowd cheering and applauding in a stadium after a goal", Duration: 8},
		{Title: "Sci-fi UI beep", Prompt: "a short futuristic sci-fi user interface beep, clean and bright", Duration: 2},
		{Title: "Footsteps on gravel", Prompt: "slow footsteps walking on gravel, crunching under each step", Duration: 8},
		{Title: "Distant thunder", Prompt: "thunder rolling far away across open countryside, low rumble fading out", Duration: 10},
	},
	Avatar: {
		{Title: "Greeting (DE)", Size: "512x640", Text: "Hallo! Schön, dass du da bist. Heute zeige ich dir etwas Neues.",
			Prompt: "A person talks warmly to the camera, natural facial expressions, the lips move in sync with the speech, subtle head movement, steady camera"},
		{Title: "Product intro", Size: "512x640", Text: "Hi, I'm here to show you how this works. It only takes a minute, and you can try it yourself right away.",
			Prompt: "A presenter speaks to the camera with a friendly smile and small hand gestures, soft studio light, steady camera"},
		{Title: "News anchor", Size: "640x512", Text: "Good evening. Here are tonight's headlines: the weather stays mild, and the city opens its new library on Saturday.",
			Prompt: "A news anchor at a desk talks to the camera, calm and serious, slight head movement, studio lighting"},
		{Title: "Quick test (fast)", Size: "256x432", Text: "Hallo, das ist ein kurzer Test.",
			Prompt: "A person talks to the camera, the lips move in sync with the speech"},
	},
	Embed: {
		{Title: "Paraphrases", Texts: []string{"Fix the login bug on Safari", "Safari users cannot sign in", "Plan the team offsite in May", "Book a venue for the offsite", "Write release notes for 2.4"}},
		{Title: "Cross-lingual", Texts: []string{"The weather is lovely today", "Das Wetter ist heute herrlich", "Il fait très beau aujourd'hui", "The quarterly report is late", "Der Quartalsbericht ist verspätet"}},
		{Title: "Topics", Texts: []string{"Kubernetes pod keeps restarting", "OOMKilled container in the cluster", "Best pizza dough recipe", "How long to proof sourdough", "Helm chart values override"}},
	},
	Classify: {
		{Title: "Eisenhower matrix", Question: "Which part of the Eisenhower matrix does this belong to?",
			Options: []Option{{"Do", "urgent and important"}, {"Schedule", "important, not urgent"}, {"Delegate", "urgent, not important"}, {"Drop", "neither urgent nor important"}},
			Texts:   []string{"Production is down for all customers", "Refactor the billing module next quarter", "Answer the vendor's survey by Friday", "Reorganise the emoji in Slack"}},
		{Title: "Sentiment", Question: "What is the sentiment of this feedback?",
			Options: []Option{{"Positive", ""}, {"Neutral", ""}, {"Negative", ""}},
			Texts:   []string{"The new dashboard is fantastic, saves me an hour a day", "It works, I guess", "Third outage this week, this is unacceptable", "Could you add a dark mode?"}},
		{Title: "Ticket routing", Question: "Which team should handle this ticket?",
			Options: []Option{{"Frontend", "UI, browser, layout"}, {"Backend", "API, database, server errors"}, {"Infra", "deployment, cluster, network"}, {"Billing", "invoices, payments"}},
			Texts:   []string{"Button overlaps the text on mobile Safari", "500 error when saving a board", "TLS certificate expired on the staging ingress", "I was charged twice in March"}},
	},
}
