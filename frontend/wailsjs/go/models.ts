export namespace downloader {
	
	export class Variant {
	    url: string;
	    bandwidth: number;
	    resolution: string;
	    codecs: string;
	    label: string;
	
	    static createFrom(source: any = {}) {
	        return new Variant(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.bandwidth = source["bandwidth"];
	        this.resolution = source["resolution"];
	        this.codecs = source["codecs"];
	        this.label = source["label"];
	    }
	}

}

export namespace main {
	
	export class DlConfig {
	    workers: number;
	    outDir: string;
	    speedLimitKBs: number;
	
	    static createFrom(source: any = {}) {
	        return new DlConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.workers = source["workers"];
	        this.outDir = source["outDir"];
	        this.speedLimitKBs = source["speedLimitKBs"];
	    }
	}
	export class TaskInfo {
	    id: string;
	    inputUrl: string;
	    mediaUrl: string;
	    referer: string;
	    cookie: string;
	    outDir: string;
	    fileName: string;
	    workers: number;
	    status: string;
	    done: number;
	    total: number;
	    output: string;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new TaskInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.inputUrl = source["inputUrl"];
	        this.mediaUrl = source["mediaUrl"];
	        this.referer = source["referer"];
	        this.cookie = source["cookie"];
	        this.outDir = source["outDir"];
	        this.fileName = source["fileName"];
	        this.workers = source["workers"];
	        this.status = source["status"];
	        this.done = source["done"];
	        this.total = source["total"];
	        this.output = source["output"];
	        this.error = source["error"];
	    }
	}

}

export namespace sniffer {
	
	export class Candidate {
	    url: string;
	    kind: string;
	    label: string;
	    referer: string;
	    userAgent: string;
	    cookie: string;
	
	    static createFrom(source: any = {}) {
	        return new Candidate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.kind = source["kind"];
	        this.label = source["label"];
	        this.referer = source["referer"];
	        this.userAgent = source["userAgent"];
	        this.cookie = source["cookie"];
	    }
	}
	export class WatchUpdate {
	    title: string;
	    candidates: Candidate[];
	    alive: boolean;
	
	    static createFrom(source: any = {}) {
	        return new WatchUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.title = source["title"];
	        this.candidates = this.convertValues(source["candidates"], Candidate);
	        this.alive = source["alive"];
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

