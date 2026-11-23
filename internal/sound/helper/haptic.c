// aivoicebus-haptic: taps the Force Touch trackpad's haptic motor, which is also how it makes a
// sound with the speakers muted (the glass moves, so the desk and the air do).
//
// Built on the Mac that uses it, once, from this file (aivoicebus sound setup), because the motor is
// reached through the private MultitouchSupport framework and aivoicebus itself is a static Go
// binary without cgo. The framework is opened at run time, so this builds on any Mac and says so
// plainly when a function is missing or there is no Force Touch trackpad.
//
// The technique (MTActuationCreateFromDictionary + MTActuationActuate, sine tones with a frequency,
// strength and length) was found by os6/nospeaker and used by asadm/mechpad (both MIT); this is an
// independent implementation of the same calls.
//
//   aivoicebus-haptic [--probe] [--level 0.1-1] ITEM...
//   ITEM  = wait:MS
//         | BASEAMP,BASEMS[:HZ,AMP,MS]...        one burst: a soft thump plus up to 4 tones
//
// Exit: 0 played (or probed), 2 usage, 3 the framework lacks the functions, 4 no trackpad,
// 5 the trackpad has no haptic motor, 6 the motor refused.

#include <CoreFoundation/CoreFoundation.h>
#include <dlfcn.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define MAX_ITEMS 12
#define MAX_TONES 4
#define MAX_TOTAL_MS 1500.0

typedef CFArrayRef (*CreateListFn)(void);
typedef int (*GetIDFn)(void *, uint64_t *);
typedef bool (*IsBuiltInFn)(void *);
typedef CFTypeRef (*ActuatorCreateFn)(uint64_t);
typedef int (*ActuatorOpenFn)(CFTypeRef);
typedef int (*ActuatorCloseFn)(CFTypeRef);
typedef void *(*ActuationCreateFn)(CFDictionaryRef, int);
typedef int (*ActuationPlayFn)(void *, void *, uint32_t, float, float);

typedef struct {
	double hz, amp, ms;
} Tone;

typedef struct {
	bool wait;
	double waitMS;
	double baseAmp, baseMS;
	int n;
	Tone tones[MAX_TONES];
} Item;

static int die(int code, const char *msg) {
	fprintf(stderr, "aivoicebus-haptic: %s\n", msg);
	return code;
}

static double clampd(double v, double lo, double hi) { return v < lo ? lo : (v > hi ? hi : v); }

// parseItem reads one argument; false when it is not in the form above.
static bool parseItem(const char *arg, Item *it) {
	memset(it, 0, sizeof *it);
	if (strncmp(arg, "wait:", 5) == 0) {
		char *end;
		it->wait = true;
		it->waitMS = strtod(arg + 5, &end);
		return *end == 0 && it->waitMS >= 0 && it->waitMS <= 500;
	}
	char *copy = strdup(arg), *save = NULL;
	if (!copy) return false;
	bool ok = false;
	char *part = strtok_r(copy, ":", &save);
	if (part && sscanf(part, "%lf,%lf", &it->baseAmp, &it->baseMS) == 2) {
		ok = true;
		while ((part = strtok_r(NULL, ":", &save)) != NULL) {
			Tone t;
			if (it->n >= MAX_TONES || sscanf(part, "%lf,%lf,%lf", &t.hz, &t.amp, &t.ms) != 3) {
				ok = false;
				break;
			}
			it->tones[it->n++] = t;
		}
	}
	free(copy);
	return ok;
}

static void put(CFMutableDictionaryRef d, const char *key, CFTypeRef value) {
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	CFDictionarySetValue(d, k, value);
	CFRelease(k);
	CFRelease(value);
}

static CFTypeRef number(double v) { return CFNumberCreate(NULL, kCFNumberDoubleType, &v); }
static CFTypeRef string(const char *s) { return CFStringCreateWithCString(NULL, s, kCFStringEncodingUTF8); }

// shape is the actuation dictionary the framework takes: a Gaussian base thump and sine tones. The
// strengths are scaled to add up to at most 1; past that the driver stalls on longer runs.
static CFDictionaryRef shape(const Item *it, double *durationMS) {
	double base = clampd(it->baseAmp, 0, 0.6), sum = 0;
	for (int i = 0; i < it->n; i++) sum += clampd(it->tones[i].amp, 0, 1);
	double scale = (sum + base > 1.0 && sum > 0) ? (1.0 - base) / sum : 1.0;
	*durationMS = clampd(it->baseMS, 1.5, 60);

	CFMutableDictionaryRef root = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFMutableDictionaryRef gauss = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	put(gauss, "Type", string("Gaussian"));
	put(gauss, "Amplitude", number(base));
	put(gauss, "DurationMS", number(*durationMS));
	put(root, "BaseWaveform", gauss);

	CFMutableArrayRef tones = CFArrayCreateMutable(NULL, 0, &kCFTypeArrayCallBacks);
	for (int i = 0; i < it->n; i++) {
		double ms = clampd(it->tones[i].ms, 1.5, 120);
		if (ms > *durationMS) *durationMS = ms;
		CFMutableDictionaryRef t = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		put(t, "Type", string("Sine"));
		put(t, "Amplitude", number(clampd(it->tones[i].amp, 0, 1) * scale));
		put(t, "DurationMS", number(ms));
		put(t, "DelayMS", number(0));
		put(t, "FrequencykHz", number(clampd(it->tones[i].hz, 95, 4000) / 1000.0));
		CFArrayAppendValue(tones, t);
		CFRelease(t);
	}
	put(root, "Tones", tones);
	return root;
}

int main(int argc, char **argv) {
	bool probe = false;
	double level = 0.8;
	Item items[MAX_ITEMS];
	int n = 0;
	double total = 0;

	for (int i = 1; i < argc; i++) {
		if (strcmp(argv[i], "--probe") == 0) {
			probe = true;
		} else if (strcmp(argv[i], "--level") == 0 && i + 1 < argc) {
			char *end;
			level = strtod(argv[++i], &end);
			if (*end || level < 0.1 || level > 1.0) return die(2, "--level is from 0.1 to 1");
		} else if (n < MAX_ITEMS && parseItem(argv[i], &items[n])) {
			total += items[n].wait ? items[n].waitMS : clampd(items[n].baseMS, 1.5, 60) + 120;
			n++;
		} else {
			return die(2, "usage: aivoicebus-haptic [--probe] [--level 0.1-1] (wait:MS | BASEAMP,BASEMS[:HZ,AMP,MS]...)...");
		}
	}
	if (!probe && n == 0) return die(2, "nothing to play");
	if (total > MAX_TOTAL_MS * 2) return die(2, "that pattern is too long");

	void *mt = dlopen("/System/Library/PrivateFrameworks/MultitouchSupport.framework/MultitouchSupport", RTLD_NOW);
	if (!mt) return die(3, "this macOS has no MultitouchSupport framework");
	CreateListFn createList = (CreateListFn)dlsym(mt, "MTDeviceCreateList");
	GetIDFn getID = (GetIDFn)dlsym(mt, "MTDeviceGetDeviceID");
	IsBuiltInFn isBuiltIn = (IsBuiltInFn)dlsym(mt, "MTDeviceIsBuiltIn");
	ActuatorCreateFn actuatorCreate = (ActuatorCreateFn)dlsym(mt, "MTActuatorCreateFromDeviceID");
	ActuatorOpenFn actuatorOpen = (ActuatorOpenFn)dlsym(mt, "MTActuatorOpen");
	ActuatorCloseFn actuatorClose = (ActuatorCloseFn)dlsym(mt, "MTActuatorClose");
	ActuationCreateFn actuationCreate = (ActuationCreateFn)dlsym(mt, "MTActuationCreateFromDictionary");
	ActuationPlayFn actuationPlay = (ActuationPlayFn)dlsym(mt, "MTActuationActuate");
	if (!createList || !getID || !actuatorCreate || !actuatorOpen || !actuationCreate || !actuationPlay)
		return die(3, "this macOS lacks the trackpad functions this needs (it is a private framework)");

	CFArrayRef list = createList();
	if (!list || CFArrayGetCount(list) == 0) return die(4, "no trackpad found");

	// The built-in trackpad first, then an external one (a Magic Trackpad).
	CFTypeRef motor = NULL;
	uint64_t motorID = 0;
	bool builtin = false, sawDevice = false;
	for (int pass = 0; pass < 2 && !motor; pass++) {
		for (CFIndex i = 0; i < CFArrayGetCount(list) && !motor; i++) {
			void *dev = (void *)CFArrayGetValueAtIndex(list, i);
			uint64_t id = 0;
			if (!dev || getID(dev, &id) != 0) continue;
			bool b = isBuiltIn ? isBuiltIn(dev) : false;
			if ((pass == 0) != b) continue;
			sawDevice = true;
			CFTypeRef ref = actuatorCreate(id);
			if (ref && actuatorOpen(ref) == 0) {
				motor = ref;
				motorID = id;
				builtin = b;
			} else if (ref) {
				CFRelease(ref);
			}
		}
	}
	if (!motor) return die(sawDevice ? 5 : 4, sawDevice ? "the trackpad has no haptic motor (it needs Force Touch)" : "no trackpad found");

	if (probe) {
		printf("trackpad id=%llu builtin=%d\n", (unsigned long long)motorID, builtin ? 1 : 0);
		if (actuatorClose) actuatorClose(motor);
		return 0;
	}

	int rc = 0;
	for (int i = 0; i < n; i++) {
		if (items[i].wait) {
			usleep((useconds_t)(items[i].waitMS * 1000));
			continue;
		}
		double ms = 0;
		CFDictionaryRef dict = shape(&items[i], &ms);
		void *act = actuationCreate(dict, 0);
		CFRelease(dict);
		if (!act) {
			rc = die(6, "the motor refused that pattern");
			break;
		}
		if (actuationPlay(act, (void *)motor, 0, (float)level, 0) != 0) rc = die(6, "the motor refused to play");
		CFRelease((CFTypeRef)act);
		usleep((useconds_t)((ms + 12) * 1000)); // let it finish before the next burst or the exit
	}
	if (actuatorClose) actuatorClose(motor);
	return rc;
}
