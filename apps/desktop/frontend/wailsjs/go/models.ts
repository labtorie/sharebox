export namespace config {
	
	export class Settings {
	    folder: string;
	    serverUrl: string;
	    permissions: fileserver.Permissions;
	    maxUploadMB: number;
	    minFreeMB: number;
	    maxConcurrent: number;
	    expiryHours: number;
	    portMapping: boolean;
	    keepRunningOnClose: boolean;
	    theme: string;
	    showQr: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.folder = source["folder"];
	        this.serverUrl = source["serverUrl"];
	        this.permissions = this.convertValues(source["permissions"], fileserver.Permissions);
	        this.maxUploadMB = source["maxUploadMB"];
	        this.minFreeMB = source["minFreeMB"];
	        this.maxConcurrent = source["maxConcurrent"];
	        this.expiryHours = source["expiryHours"];
	        this.portMapping = source["portMapping"];
	        this.keepRunningOnClose = source["keepRunningOnClose"];
	        this.theme = source["theme"];
	        this.showQr = source["showQr"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace fileserver {
	
	export class Permissions {
	    download: boolean;
	    upload: boolean;
	    mkdir: boolean;
	    delete: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Permissions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.download = source["download"];
	        this.upload = source["upload"];
	        this.mkdir = source["mkdir"];
	        this.delete = source["delete"];
	    }
	}

}

export namespace host {
	
	export class Check {
	    state: string;
	    detail: string;
	
	    static createFrom(source: any = {}) {
	        return new Check(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.detail = source["detail"];
	    }
	}
	export class FolderInfo {
	    path: string;
	    name: string;
	    size: number;
	    files: number;
	    scanning: boolean;
	    partial: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FolderInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.files = source["files"];
	        this.scanning = source["scanning"];
	        this.partial = source["partial"];
	    }
	}
	export class Status {
	    state: string;
	    link?: string;
	    linkReady: boolean;
	    headline: string;
	    server: Check;
	    direct: Check;
	    p2p: Check;
	    relay: Check;
	    mapping: Check;
	    errors: string[];
	    folder: FolderInfo;
	    stats: transfers.Snapshot;
	    // Go type: time
	    started: any;
	    // Go type: time
	    expiresAt: any;
	    previewUrl?: string;
	    password: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.link = source["link"];
	        this.linkReady = source["linkReady"];
	        this.headline = source["headline"];
	        this.server = this.convertValues(source["server"], Check);
	        this.direct = this.convertValues(source["direct"], Check);
	        this.p2p = this.convertValues(source["p2p"], Check);
	        this.relay = this.convertValues(source["relay"], Check);
	        this.mapping = this.convertValues(source["mapping"], Check);
	        this.errors = source["errors"];
	        this.folder = this.convertValues(source["folder"], FolderInfo);
	        this.stats = this.convertValues(source["stats"], transfers.Snapshot);
	        this.started = this.convertValues(source["started"], null);
	        this.expiresAt = this.convertValues(source["expiresAt"], null);
	        this.previewUrl = source["previewUrl"];
	        this.password = source["password"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace transfers {
	
	export class Guest {
	    id: string;
	    transport: string;
	    route: string;
	    // Go type: time
	    since: any;
	    // Go type: time
	    lastSeen: any;
	    label: string;
	
	    static createFrom(source: any = {}) {
	        return new Guest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.transport = source["transport"];
	        this.route = source["route"];
	        this.since = this.convertValues(source["since"], null);
	        this.lastSeen = this.convertValues(source["lastSeen"], null);
	        this.label = source["label"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Transfer {
	    id: string;
	    guest: string;
	    kind: string;
	    name: string;
	    total: number;
	    bytes: number;
	    speed: number;
	    status: string;
	    error?: string;
	    transport: string;
	    // Go type: time
	    started: any;
	    // Go type: time
	    ended: any;
	
	    static createFrom(source: any = {}) {
	        return new Transfer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.guest = source["guest"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.total = source["total"];
	        this.bytes = source["bytes"];
	        this.speed = source["speed"];
	        this.status = source["status"];
	        this.error = source["error"];
	        this.transport = source["transport"];
	        this.started = this.convertValues(source["started"], null);
	        this.ended = this.convertValues(source["ended"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Snapshot {
	    active: Transfer[];
	    recent: Transfer[];
	    guests: Guest[];
	    upSpeed: number;
	    downSpeed: number;
	    totalUp: number;
	    totalDown: number;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.active = this.convertValues(source["active"], Transfer);
	        this.recent = this.convertValues(source["recent"], Transfer);
	        this.guests = this.convertValues(source["guests"], Guest);
	        this.upSpeed = source["upSpeed"];
	        this.downSpeed = source["downSpeed"];
	        this.totalUp = source["totalUp"];
	        this.totalDown = source["totalDown"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

