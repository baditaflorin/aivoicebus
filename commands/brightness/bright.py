import ctypes, sys
v = float(sys.argv[1]) if len(sys.argv) > 1 else 1.0
cg = ctypes.CDLL('/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics')
ds = ctypes.CDLL('/System/Library/PrivateFrameworks/DisplayServices.framework/DisplayServices')
cg.CGMainDisplayID.restype = ctypes.c_uint32
ds.DisplayServicesSetBrightness.argtypes = [ctypes.c_uint32, ctypes.c_float]
ds.DisplayServicesGetBrightness.argtypes = [ctypes.c_uint32, ctypes.POINTER(ctypes.c_float)]
d = cg.CGMainDisplayID()
r = ds.DisplayServicesSetBrightness(d, v)
b = ctypes.c_float(); ds.DisplayServicesGetBrightness(d, ctypes.byref(b))
print("set rc=%d brightness now=%.2f" % (r, b.value))
